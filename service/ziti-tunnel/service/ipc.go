/*
 * Copyright NetFoundry, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package service

import "C"
import (
	"bufio"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/Microsoft/go-winio"
	"github.com/openziti/desktop-edge-win/service/cziti"
	"github.com/openziti/desktop-edge-win/service/windns"
	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/config"
	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/constants"
	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/dto"
	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/util/logging"
	"github.com/openziti/foundation/identity/identity"
	idcfg "github.com/openziti/sdk-golang/ziti/config"
	"github.com/openziti/sdk-golang/ziti/enroll"
	"golang.org/x/sys/windows/svc"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Pipes struct {
	ipc    net.Listener
	logs   net.Listener
	events net.Listener
}

func (p *Pipes) Close() {
	_ = p.ipc.Close()
	_ = p.logs.Close()
	_ = p.events.Close()
}

var shutdown = make(chan bool, 8) //a channel informing go routines to exit
var notificationFrequency *time.Ticker

func SubMain(ops chan string, changes chan<- svc.Status, winEvents <-chan WindowsEvents) error {
	log.Info("============================== service begins ==============================")
	windns.RemoveAllNrptRules()
	// cleanup old ziti tun profiles
	windns.CleanUpNetworkAdapterProfile()
	CleanUpZitiTUNAdapters(TunName)

	rts.LoadConfig()
	l := rts.state.LogLevel
	parsedLevel, cLogLevel := logging.ParseLevel(l)

	rts.state.LogLevel = parsedLevel.String()
	logging.InitLogger(parsedLevel)
	_ = logging.Elog.Info(InformationEvent, SvcName+" starting. log file located at "+config.LogFile())

	if rts.state.ApiPageSize < constants.MinimumApiPageSize {
		log.Debugf("page size value was smaller than the minimim %d. using default page size: %d", constants.MinimumApiPageSize, constants.DefaultApiPageSize)
		rts.state.ApiPageSize = constants.DefaultApiPageSize
	}

	// create a channel for notifying any connections that they are to be interrupted
	interrupt = make(chan struct{}, 8)

	// a channel to signal the handleEvents that initialization is complete
	initialized := make(chan struct{})
	shutdownDelay := make(chan bool)

	defer rts.Close()
	// initialize the network interface
	err := initialize(cLogLevel)
	if err != nil {
		log.Panicf("unexpected err from initialize: %v", err)
		return err
	}

	TunStarted = time.Now()

	for _, id := range rts.ids {
		connectIdentity(id)
	}

	go handleEvents(initialized)

	//listen for services that show up
	go acceptServices()

	// listen for windows power events and call mfa auth func
	go func() {
		for {
			select {
			case wEvents := <-winEvents:
				if wEvents.WinPowerEvent == PBT_APMRESUMESUSPEND || wEvents.WinPowerEvent == PBT_APMRESUMEAUTOMATIC {
					log.Debugf("Received Windows Power Event in tunnel %d", wEvents.WinPowerEvent)
					for _, id := range rts.ids {
						if id.CId != nil && id.CId.Loaded {
							cziti.EndpointStateChanged(id.CId, true, false)
						}
					}
				}
				if wEvents.WinSessionEvent == WTS_SESSION_UNLOCK {
					log.Debugf("Received Windows Session Event (device unlocked) in tunnel %d", wEvents.WinSessionEvent)
					for _, id := range rts.ids {
						if id.CId != nil && id.CId.Loaded {
							cziti.EndpointStateChanged(id.CId, false, true)
						}
					}
				}
				rts.BroadcastEvent(dto.TunnelStatusEvent{
					StatusEvent: dto.StatusEvent{Op: "status"},
					Status:      rts.ToStatus(true),
					ApiVersion:  API_VERSION,
				})
				broadcastNotification(true)
			case <-shutdownDelay:
				log.Tracef("Exiting windows power events loop")
				return
			}
		}
	}()

	// open the pipe for business
	pipes, err := openPipes()
	if err != nil {
		return err
	}
	defer pipes.Close()

	// notify the service is running
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
	_ = logging.Elog.Info(InformationEvent, SvcName+" status set to running")
	log.Info(SvcName + " status set to running. starting cancel loop")

	rts.SaveState() //if we get this far it means things seem to be working. backup the config

	//indicate the metrics handler can begin
	initialized <- struct{}{}

	waitForStopRequest(ops)

	log.Debug("shutting down. start a ZitiDump")
	for _, id := range rts.ids {
		if id.CId != nil && id.CId.Loaded {
			cziti.ZitiDumpOnShutdown(id.CId)
		}
	}
	log.Debug("shutting down. ZitiDump complete")

	requestShutdown("service shutdown")

	// signal to any connected consumers that the service is shutting down normally
	rts.BroadcastEvent(dto.StatusEvent{
		Op: "shutdown",
	})

	// wait 1 second for the shutdown to send to clients
	go func() {
		time.Sleep(1 * time.Second)
		close(shutdownDelay)
	}()
	<-shutdownDelay

	windns.RemoveAllNrptRules()

	log.Infof("shutting down connections...")
	pipes.shutdownConnections()

	log.Infof("shutting down events...")
	events.shutdown()

	log.Infof("Closing connections and removing tun interface: %s", TunName)
	rts.Close()

	log.Info("==============================  service ends  ==============================")

	ops <- "done"
	return nil
}

func requestShutdown(requester string) {
	log.Infof("shutdown requested by %v", requester)
	close(shutdown) // stops the metrics ticker, service change listener and notification alert loop
}

func waitForStopRequest(ops <-chan string) {
	sig := make(chan os.Signal)
	signal.Notify(sig)
loop:
	for {
		select {
		case c := <-ops:
			log.Infof("request for control received: %v", c)
			if c == "stop" {
				break loop
			} else {
				log.Debug("unexpected operation: " + c)
			}
		case s := <-sig:
			log.Warnf("signal received! %v", s)
		}
	}
	log.Debugf("wait loop is exiting")
}

func openPipes() (*Pipes, error) {
	// create the ACE string representing the following groups have access to the pipes created
	grps := []string{InteractivelyLoggedInUser, System, BuiltinAdmins, LocalService}
	auth := "D:" + strings.Join(grps, "")

	// create the pipes
	pc := winio.PipeConfig{
		SecurityDescriptor: auth,
		MessageMode:        false,
		InputBufferSize:    1024,
		OutputBufferSize:   1024,
	}
	logs, err := winio.ListenPipe(logsPipeName(), &pc)
	if err != nil {
		return nil, err
	}
	ipc, err := winio.ListenPipe(IpcPipeName(), &pc)
	if err != nil {
		return nil, err
	}
	events, err := winio.ListenPipe(eventsPipeName(), &pc)
	if err != nil {
		return nil, err
	}

	// listen for log requests
	go accept(logs, serveLogs, "  logs")
	log.Debugf("log listener ready. pipe: %s", logsPipeName())

	// listen for ipc messages
	go accept(ipc, serveIpc, "   ipc")
	log.Debugf("ipc listener ready pipe: %s", IpcPipeName())

	// listen for events messages
	go accept(events, serveEvents, "events")
	log.Debugf("events listener ready pipe: %s", eventsPipeName())

	return &Pipes{
		ipc:    ipc,
		logs:   logs,
		events: events,
	}, nil
}

func (p *Pipes) shutdownConnections() {
	log.Info("waiting for all connections to close...")
	p.Close()

	for i := 0; i < ipcConnections; i++ {
		log.Debug("cancelling ipc read loop...")
		interrupt <- struct{}{}
	}
	log.Info("waiting for all ipc connections to close...")
	ipcWg.Wait()
	log.Info("all ipc connections closed")

	for i := 0; i < eventsConnections; i++ {
		log.Debug("cancelling events read loop...")
		interrupt <- struct{}{}
	}
	log.Info("waiting for all events connections to close...")
	eventsWg.Wait()
	log.Info("all events connections closed")
}

func initialize(cLogLevel int) error {
	//TODO: this all needs to be cleaned up. it's done it two places and redundant
	//TODO: fix with mfa?
	ipv4 := rts.state.TunIpv4
	ipv4mask := rts.state.TunIpv4Mask
	if strings.TrimSpace(ipv4) == "" {
		log.Infof("ip not provided using default: %v", ipv4)
		ipv4 = constants.Ipv4ip
		rts.UpdateIpv4(ipv4)
	}
	if ipv4mask < constants.Ipv4MaxMask {
		log.Warnf("provided mask is too large: %d using default: %d", ipv4mask, constants.Ipv4DefaultMask)
		ipv4mask = constants.Ipv4DefaultMask
		rts.UpdateIpv4Mask(ipv4mask)
	}
	_, ipnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ipv4, ipv4mask))
	if err != nil {
		return fmt.Errorf("error parsing CIDR block: (%v)", err)
	}
	dnsIpAsUint32 := binary.BigEndian.Uint32(ipnet.IP)
	cziti.InitTunnelerDns(dnsIpAsUint32, len(ipnet.Mask))

	assignedIp, t, err := rts.CreateTun(rts.state.TunIpv4, rts.state.TunIpv4Mask, rts.state.AddDns)
	if err != nil {
		return err
	}

	cziti.Start(rts, rts.state.TunIpv4, rts.state.TunIpv4Mask, cLogLevel)
	err = cziti.HookupTun(*t)
	if err != nil {
		log.Panicf("An unrecoverable error has occurred! %v", err)
	}

	setTunInfo(rts.state)

	rts.state.Active = true
	for _, id := range rts.state.Identities {
		if id != nil {
			i := &Id{
				Identity: *id,
				CId:      nil,
			}
			rts.ids[id.FingerPrint] = i
		} else {
			log.Warnf("identity was nil?")
		}
	}
	dnsReady := make(chan bool)
	go cziti.RunDNSserver([]net.IP{assignedIp}, dnsReady)
	<-dnsReady
	log.Debugf("initial state loaded from configuration file")
	return nil
}

func setTunInfo(s *dto.TunnelStatus) {
	ipv4 := rts.state.TunIpv4
	ipv4mask := rts.state.TunIpv4Mask

	if strings.TrimSpace(ipv4) == "" {
		ipv4 = constants.Ipv4ip
		log.Infof("ip not provided in config file. using default: %v", ipv4)
		rts.UpdateIpv4(ipv4)
	}
	if ipv4mask < constants.Ipv4MaxMask || ipv4mask > constants.Ipv4MinMask {
		log.Warnf("provided mask is invalid: %d. using default value: %d", ipv4mask, constants.Ipv4DefaultMask)
		ipv4mask = constants.Ipv4DefaultMask
		rts.UpdateIpv4Mask(ipv4mask)
	}
	_, ipnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ipv4, ipv4mask))
	if err != nil {
		log.Errorf("error parsing CIDR block: (%v)", err)
		return
	}

	carrierGradeNetworkRange := fmt.Sprintf("%s/%d", constants.Ipv4ip, 10)
	_, cgsubnet, _ := net.ParseCIDR(carrierGradeNetworkRange)

	if !cgsubnet.Contains(ipnet.IP) {
		log.Warnf("============================================================================")
		log.Warnf("the ip provided [%s] is NOT in the expected CIDR range of %s", ipv4, carrierGradeNetworkRange)
		log.Warnf("this can cause unintented consequences. Please update the ip to one in the specified range.")
		log.Warnf("============================================================================")
	}

	t := *rts.tun
	mtu, err := t.MTU()
	if err != nil {
		log.Errorf("error reading MTU - using 0 for MTU: (%v)", err)
		mtu = 0
	}
	umtu := uint16(mtu)
	//set the tun info into the state
	s.IpInfo = &dto.TunIpInfo{
		Ip:     ipv4,
		DNS:    ipv4,
		MTU:    umtu,
		Subnet: ipv4MaskString(ipnet.Mask),
	}
}

func ipv4MaskString(m []byte) string {
	if len(m) != 4 {
		log.Panicf("An unexpected and unrecoverable error has occurred. ipv4Mask: len must be 4 bytes")
	}

	return fmt.Sprintf("%d.%d.%d.%d", m[0], m[1], m[2], m[3])
}

func closeConn(conn net.Conn) {
	err := conn.Close()
	if err != nil {
		log.Warnf("abnormal error while closing connection. %v", err)
	}
}

func accept(p net.Listener, serveFunction func(net.Conn), debug string) {
	for {
		c, err := p.Accept()
		if err != nil {
			if err != winio.ErrPipeListenerClosed {
				log.Errorf("%v unexpected error while accepting a connection. exiting loop. %v", debug, err)
			}
			return
		}

		go serveFunction(c)
	}
}

func serveIpc(conn net.Conn) {
	log.Debug("beginning ipc receive loop")
	defer log.Info("a connected IPC client has disconnected")
	defer closeConn(conn) //close the connection after this function invoked as go routine exits

	done := make(chan struct{}, 8)
	defer close(done) // ensure that goroutine exits

	ipcWg.Add(1)
	ipcConnections++
	defer func() {
		log.Debugf("serveIpc is exiting. total connection count now: %d", ipcConnections)
		ipcWg.Done()
		ipcConnections--
		log.Debugf("serveIpc is exiting. total connection count now: %d", ipcConnections)
	}() // count down whenever the function exits
	log.Debugf("accepting a new client for serveIpc. total connection count: %d", ipcConnections)

	go func() {
		select {
		case <-interrupt:
			log.Info("request to interrupt read loop received")
			_ = conn.Close()
			log.Info("read loop interrupted")
		case <-done:
			log.Debug("loop finished normally")
		}
	}()

	writer := bufio.NewWriter(conn)
	reader := bufio.NewReader(conn)
	rw := bufio.NewReadWriter(reader, writer)
	enc := json.NewEncoder(writer)

	for {
		log.Trace("ipc read begins")
		msg, readErr := reader.ReadString('\n')
		log.Trace("ipc read ends")
		if readErr != nil {
			if readErr != winio.ErrFileClosed {
				if readErr == io.EOF {
					log.Debug("pipe closed. client likely disconnected")
				} else {
					log.Errorf("unexpected error while reading line. %v", readErr)
				}
			}

			//try to respond... likely won't work but try...
			respondWithError(enc, "connection closed due to shutdown request for ipc", UNKNOWN_ERROR, readErr)
			log.Debugf("connection closed due to shutdown request for ipc: %v", readErr)
			return
		}

		log.Debugf("msg received: %s", msg)

		if strings.TrimSpace(msg) == "" {
			// empty message. ignore it and read again
			log.Debug("empty line received. ignoring")
			continue
		}

		dec := json.NewDecoder(strings.NewReader(msg))
		var cmd dto.CommandMsg
		if cmdErr := dec.Decode(&cmd); cmdErr == io.EOF {
			respondWithError(enc, "could not decode command properly", UNKNOWN_ERROR, cmdErr)
			continue
		} else if cmdErr != nil {
			log.Error(cmdErr)
			respondWithError(enc, "could not decode command properly", UNKNOWN_ERROR, cmdErr)
			continue
		}

		switch cmd.Function {
		case "AddIdentity":
			addIdMsg, addErr := reader.ReadString('\n')
			if addErr != nil {
				respondWithError(enc, "could not read string properly", UNKNOWN_ERROR, addErr)
				break
			}
			log.Debugf("AddIdentity msg received: %s", addIdMsg)
			addIdDec := json.NewDecoder(strings.NewReader(addIdMsg))

			var newId dto.AddIdentity
			if err := addIdDec.Decode(&newId); err == io.EOF {
				respondWithError(enc, "could not decode string properly", UNKNOWN_ERROR, err)
				break
			} else if err != nil {
				log.Error(err)
				respondWithError(enc, "could not decode string properly", UNKNOWN_ERROR, err)
				break
			}
			newIdentity(newId, enc)

			//save the state
			rts.SaveState()
		case "RemoveIdentity":
			log.Debugf("Request received to remove an identity")
			removeIdentity(enc, cmd.Payload["Fingerprint"].(string))

			//save the state
			rts.SaveState()
		case "Status":
			reportStatus(enc)
		case "IdentityOnOff":
			onOff := cmd.Payload["OnOff"].(bool)
			fingerprint := cmd.Payload["Fingerprint"].(string)
			toggleIdentity(enc, fingerprint, onOff)

			//save the state
			rts.SaveState()
		case "SetLogLevel":
			setLogLevel(enc, cmd.Payload["Level"].(string))

			//save the state
			rts.SaveState()
		case "UpdateTunIpv4":
			var tunIPv4 string
			var tunIPv4Mask int
			var addDns string
			var providedPageSize int
			if cmd.Payload["TunIPv4"] != nil {
				tunIPv4 = cmd.Payload["TunIPv4"].(string)
			}
			if cmd.Payload["TunIPv4Mask"] != nil {
				var tunIpv4Mask = cmd.Payload["TunIPv4Mask"].(float64)
				tunIPv4Mask = int(tunIpv4Mask)
			}
			if cmd.Payload["AddDns"] != nil {
				addDns = strconv.FormatBool(cmd.Payload["AddDns"].(bool))
			}
			if cmd.Payload["ApiPageSize"] != nil {
				ps := cmd.Payload["ApiPageSize"].(float64)
				providedPageSize = int(ps)
			}
			updateTunIpv4(enc, tunIPv4, tunIPv4Mask, addDns, providedPageSize)
		case "NotifyLogLevelUIAndUpdateService":
			sendLogLevelAndNotify(enc, cmd.Payload["Level"].(string))
		case "NotifyIdentityUI":
			sendIdentityAndNotifyUI(enc, cmd.Payload["Fingerprint"].(string))
		case "ZitiDump":
			log.Debug("request to ZitiDump received")
			for _, id := range rts.ids {
				if id.CId != nil {
					cziti.ZitiDump(id.CId, fmt.Sprintf(`%s\%s.ziti.txt`, config.LogsPath(), id.Name))
				}
			}
			log.Debug("request to ZitiDump complete")
			respond(enc, dto.Response{Message: "ZitiDump complete", Code: SUCCESS, Error: "", Payload: nil})
		case "EnableMFA":
			fingerprint := cmd.Payload["Fingerprint"].(string)
			enableMfa(enc, fingerprint)
		case "VerifyMFA":
			fingerprint := cmd.Payload["Fingerprint"].(string)
			code := cmd.Payload["Code"].(string)
			verifyMfa(enc, fingerprint, code)
		case "AuthMFA":
			fingerprint := cmd.Payload["Fingerprint"].(string)
			code := cmd.Payload["Code"].(string)
			authMfa(enc, fingerprint, code)
		case "ReturnMFACodes":
			fingerprint := cmd.Payload["Fingerprint"].(string)
			code := cmd.Payload["Code"].(string)
			returnMfaCodes(enc, fingerprint, code)
		case "GenerateMFACodes":
			fingerprint := cmd.Payload["Fingerprint"].(string)
			code := cmd.Payload["Code"].(string)
			generateMfaCodes(enc, fingerprint, code)
		case "RemoveMFA":
			fingerprint := cmd.Payload["Fingerprint"].(string)
			code := cmd.Payload["Code"].(string)
			removeMFA(enc, fingerprint, code)
		case "UpdateFrequency":
			notificationFreq := cmd.Payload["NotificationFrequency"].(float64)
			updateNotificationFrequency(enc, int(notificationFreq))
		case "Debug":
			dbg()
			respond(enc, dto.Response{
				Code:    0,
				Message: "debug",
				Error:   "debug",
				Payload: nil,
			})

			//save the state
			rts.SaveState()
		default:
			log.Warnf("Unknown operation: %s. Returning error on pipe", cmd.Function)
			respondWithError(enc, "Something unexpected has happened", UNKNOWN_ERROR, nil)
		}

		_ = rw.Flush()
	}
}

func generateMfaCodes(out *json.Encoder, fingerprint string, code string) {
	id := rts.Find(fingerprint)
	if id != nil {
		codes, err := cziti.GenerateMfaCodes(id.CId, code)
		if err == nil {
			respond(out, dto.Response{Message: "success", Code: SUCCESS, Error: "", Payload: codes})
		} else {
			respondWithError(out, "msg", MFA_FAILED_TO_RETURN_CODES, err)
		}
	} else {
		respondWithError(out, "Could not generate mfa codes", MFA_FINGERPRINT_NOT_FOUND, fmt.Errorf("id not found with fingerprint: %s", fingerprint))
	}
}

func returnMfaCodes(out *json.Encoder, fingerprint string, code string) {
	id := rts.Find(fingerprint)
	if id != nil {
		codes, err := cziti.ReturnMfaCodes(id.CId, code)
		if err == nil {
			respond(out, dto.Response{Message: "success", Code: SUCCESS, Error: "", Payload: codes})
		} else {
			log.Warnf("could not return mfa codes? %v", err)
			respondWithError(out, "could not return mfa codes", MFA_FAILED_TO_RETURN_CODES, err)
		}
	} else {
		respondWithError(out, "Could not return mfa codes", MFA_FINGERPRINT_NOT_FOUND, fmt.Errorf("id not found with fingerprint: %s", fingerprint))
	}
}

func enableMfa(out *json.Encoder, fingerprint string) {
	id := rts.Find(fingerprint)
	if id != nil {
		cziti.EnableMFA(id.CId)
		respond(out, dto.Response{Message: "mfa enroll complete", Code: SUCCESS, Error: "", Payload: nil})
	} else {
		respondWithError(out, "Could not enable mfa", MFA_FINGERPRINT_NOT_FOUND, fmt.Errorf("id not found with fingerprint: %s", fingerprint))
	}
}

func verifyMfa(out *json.Encoder, fingerprint string, code string) {
	id := rts.Find(fingerprint)
	if id != nil {
		result := cziti.VerifyMFA(id.CId, code)
		if result == nil {
			// respond with auth success message immediately
			respond(out, dto.Response{Message: "mfa verify complete", Code: SUCCESS, Error: "", Payload: nil})
			id.CId.MfaEnabled = true
			rts.SetNotified(id.FingerPrint, false)
			id.CId.UpdateMFATimeRem()
			rts.BroadcastEvent(dto.IdentityEvent{
				ActionEvent: dto.IdentityUpdateComplete,
				Id:          Clean(id),
			})
		} else {
			respondWithError(out, "Could not verify mfa code", UNKNOWN_ERROR, fmt.Errorf("verification failed for fingerprint: %s", fingerprint))
		}
	} else {
		respondWithError(out, "Could not verify mfa code", MFA_FINGERPRINT_NOT_FOUND, fmt.Errorf("id not found with fingerprint: %s", fingerprint))
	}
}

func removeMFA(out *json.Encoder, fingerprint string, code string) {
	id := rts.Find(fingerprint)
	if id != nil {
		cziti.RemoveMFA(id.CId, code)
		respond(out, dto.Response{Message: "mfa removed successfully", Code: SUCCESS, Error: "", Payload: nil})
	} else {
		respondWithError(out, "Could not remove mfa", MFA_FINGERPRINT_NOT_FOUND, fmt.Errorf("id not found with fingerprint: %s", fingerprint))
	}
}

func setLogLevel(out *json.Encoder, level string) {
	goLevel, cLevel := logging.ParseLevel(level)
	log.Infof("Setting logger levels to %s", goLevel)
	logging.SetLoggingLevel(goLevel)
	cziti.SetLogLevel(cLevel)
	rts.state.LogLevel = goLevel.String()
	respond(out, dto.Response{Message: "log level set", Code: SUCCESS, Error: "", Payload: nil})
}

func updateTunIpv4(out *json.Encoder, ip string, ipMask int, addDns string, apiPageSize int) {

	err := UpdateRuntimeStateIpv4(ip, ipMask, addDns, apiPageSize)
	if err != nil {
		respondWithError(out, "Could not set Tun ip and mask", UNKNOWN_ERROR, err)
		return
	}

	respond(out, dto.Response{Message: "TunIPv4 and mask is set, Manual Restart is required", Code: SUCCESS, Error: "", Payload: ""})
}

func serveLogs(conn net.Conn) {
	log.Debug("accepted a logs connection, writing logs to pipe")
	w := bufio.NewWriter(conn)

	file, err := os.OpenFile(config.LogFile(), os.O_RDONLY, 0644)
	if err != nil {
		log.Errorf("could not open log file at %s", config.LogFile())
		_, _ = w.WriteString("an unexpected error occurred while retrieving logs. look at the actual log file.")
		return
	}
	writeLogToStream(file, w)

	err = conn.Close()
	if err != nil {
		log.Error("error closing connection", err)
	}
}

func writeLogToStream(file *os.File, writer *bufio.Writer) {
	r := bufio.NewReader(file)
	wrote, err := io.Copy(writer, r)
	if err != nil {
		log.Errorf("problem responding with log data for: %v", file)
	}
	_, err = writer.Write([]byte("end of logs\n"))
	if err != nil {
		log.Errorf("unexpected error writing log response: %v", err)
	}

	err = writer.Flush()
	if err != nil {
		log.Errorf("unexpected error flushing log response: %v", err)
	}
	log.Debugf("wrote %d bytes to client from logs", wrote)

	err = file.Close()
	if err != nil {
		log.Error("error closing log file", err)
	}
}

func serveEvents(conn net.Conn) {
	randomInt := rand.Int()
	log.Debug("accepted an events connection, writing events to pipe")
	defer closeConn(conn) //close the connection after this function invoked as go routine exits

	eventsWg.Add(1)
	eventsConnections++
	defer func() {
		log.Debugf("serveEvents is exiting. total connection count now: %d", eventsConnections)
		eventsWg.Done()
		eventsConnections--
		log.Debugf("serveEvents is exiting. total connection count now: %d", eventsConnections)
	}() // count down whenever the function exits
	log.Debugf("accepting a new client for serveEvents. total connection count: %d", eventsConnections)

	consumer := make(chan interface{}, 8)
	id := fmt.Sprintf("serveEvents:%d", randomInt)
	events.register(id, consumer)
	defer events.unregister(id)

	w := bufio.NewWriter(conn)
	o := json.NewEncoder(w)

	log.Info("new event client connected - sending current status")
	err := o.Encode(dto.TunnelStatusEvent{
		StatusEvent: dto.StatusEvent{Op: "status"},
		Status:      rts.ToStatus(true),
		ApiVersion:  API_VERSION,
	})

	if err != nil {
		log.Errorf("could not send status to event client: %v", err)
	} else {
		log.Info("status sent. listening for new events")
	}

loop:
	for {
		select {
		case msg := <-consumer:
			t := reflect.TypeOf(msg)
			log.Tracef("sending event to id: %s [%v]", id, t.Name())
			eerr := o.Encode(msg)
			if eerr != nil {
				log.Warnf("exiting from serveEvents due to error: %v", eerr)
				break loop
			}
			ferr := w.Flush()
			if ferr != nil {
				log.Warnf("flush error: %v", ferr)
				return
			}
			log.Tracef("send event to id complete: %s [%v]", id, t.Name())
		case <-interrupt:
			break loop
		}
	}
	log.Info("a connected event client has disconnected")
}

func writerFlush(writer bufio.Writer) {
	writer.Flush()
}

func reportStatus(out *json.Encoder) {
	s := rts.ToStatus(true)
	respond(out, dto.ZitiTunnelStatus{
		Status:  &s,
		Metrics: nil,
	})

	log.Debugf("request for status responded to")
}

func toggleIdentity(out *json.Encoder, fingerprint string, onOff bool) {
	log.Debugf("toggle ziti on/off for %s: %t", fingerprint, onOff)

	id := rts.Find(fingerprint)

	if id == nil {
		msg := fmt.Sprintf("identity with fingerprint %s not found", fingerprint)
		log.Warn(msg)
		respond(out, dto.Response{
			Code:    IDENTITY_NOT_FOUND,
			Message: fmt.Sprintf("no update performed. %s", msg),
			Error:   "",
			Payload: nil,
		})
	} else if id.Active == onOff {
		log.Debugf("nothing to do - the provided identity %s is already set to active=%t", id.Name, id.Active)
		//nothing to do...
		respond(out, dto.Response{
			Code:    SUCCESS,
			Message: fmt.Sprintf("no update performed. identity is already set to active=%t", onOff),
			Error:   "",
			Payload: nil,
		})
	} else {
		if onOff {
			connectIdentity(id)
		} else {
			err := disconnectIdentity(id)
			if err != nil {
				log.Warnf("could not disconnect identity: %v", err)
			}
		}
		id.Active = onOff
		rts.SaveState()
		respond(out, dto.Response{Message: "identity toggled", Code: SUCCESS, Error: "", Payload: Clean(id)})
	}

	log.Debugf("toggle ziti on/off for %s: %t responded to", fingerprint, onOff)
}

func removeTempFile(file os.File) {
	err := os.Remove(file.Name()) // clean up
	if err != nil {
		log.Warnf("could not remove temp file: %s", file.Name())
	}
	err = file.Close()
	if err != nil {
		log.Warnf("could not close the temp file: %s", file.Name())
	}
}

func newIdentity(newId dto.AddIdentity, out *json.Encoder) {
	log.Debugf("new identity for %s: %s", newId.Id.Name, newId.EnrollmentFlags.JwtString)

	tokenStr := newId.EnrollmentFlags.JwtString
	log.Debugf("jwt to parse: %s", tokenStr)
	tkn, _, err := enroll.ParseToken(tokenStr)

	if err != nil {
		respondWithError(out, "failed to parse JWT: %s", COULD_NOT_ENROLL, err)
		return
	}
	var certPath = ""
	var keyPath = ""
	var caOverride = ""

	flags := enroll.EnrollmentFlags{
		CertFile:      certPath,
		KeyFile:       keyPath,
		KeyAlg:        "EC",
		Token:         tkn,
		IDName:        newId.Id.Name,
		AdditionalCAs: caOverride,
	}

	//enroll identity using the file and go sdk
	conf, err := enroll.Enroll(flags)
	if err != nil {
		respondWithError(out, "failed to enroll", COULD_NOT_ENROLL, err)
		return
	}

	enrolled, err := ioutil.TempFile("" /*temp dir*/, "ziti-enrollment-*")
	if err != nil {
		respondWithError(out, "Could not create temporary file in local storage. This is abnormal. "+
			"Check the process has access to the temporary folder", COULD_NOT_WRITE_FILE, err)
		return
	}

	enc := json.NewEncoder(enrolled)
	enc.SetEscapeHTML(false)
	encErr := enc.Encode(&conf)

	outpath := enrolled.Name()
	if encErr != nil {
		respondWithError(out, fmt.Sprintf("enrollment successful but the identity file was not able to be written to: %s [%s]", outpath, encErr), COULD_NOT_ENROLL, err)
		return
	}

	sdkId, err := identity.LoadIdentity(conf.ID)
	if err != nil {
		respondWithError(out, "unable to load identity which was just created. this is abnormal", COULD_NOT_ENROLL, err)
		return
	}

	//map fields onto new identity
	newId.Id.Config.ZtAPI = conf.ZtAPI
	newId.Id.Config.ID = conf.ID
	newId.Id.FingerPrint = fmt.Sprintf("%x", sha1.Sum(sdkId.Cert().Leaf.Raw)) //generate fingerprint
	if newId.Id.Name == "" {
		newId.Id.Name = newId.Id.FingerPrint
	}
	newId.Id.Status = STATUS_ENROLLED

	err = enrolled.Close()
	if err != nil {
		log.Panicf("An unexpected and unrecoverable error has occurred while %s: %v", "enrolling an identity", err)
	}
	newPath := newId.Id.Path()

	//move the temp file to its final home after enrollment
	err = os.Rename(enrolled.Name(), newPath)
	if err != nil {
		log.Errorf("unexpected issue renaming the enrollment! attempting to remove the temporary file at: %s", enrolled.Name())
		removeTempFile(*enrolled)
		respondWithError(out, "a problem occurred while writing the identity file.", COULD_NOT_ENROLL, err)
	}

	//newId.Id.Active = false //set to false by default - enable the id after persisting
	log.Infof("enrolled successfully. identity file written to: %s", newPath)

	id := &Id{
		Identity: dto.Identity{
			FingerPrint: newId.Id.FingerPrint,
		},
	}

	rts.ids[id.FingerPrint] = id
	id.Active = true //since it's a new id being added - presume that it's active
	connectIdentity(id)

	state := rts.state
	//if successful parse the output and add the config to the identity
	state.Identities = append(state.Identities, &newId.Id)

	//return successful message
	resp := dto.Response{Message: "success", Code: SUCCESS, Error: "", Payload: Clean(id)}

	respond(out, resp)
	log.Debugf("new identity for %s responded to", newId.Id.Name)
}

func respondWithError(out *json.Encoder, msg string, code int, err error) {
	if err != nil {
		respond(out, dto.Response{Message: msg, Code: code, Error: err.Error()})
	} else {
		respond(out, dto.Response{Message: msg, Code: code, Error: ""})
	}
	log.Debugf("responded with error: %s, %d, %v", msg, code, err)
}

func connectIdentity(id *Id) {
	log.Infof("connecting identity: %s[%s]", id.Name, id.FingerPrint)

	if id.CId == nil || !id.CId.Loaded {
		rts.LoadIdentity(id, DEFAULT_REFRESH_INTERVAL)
		rts.BroadcastEvent(dto.IdentityEvent{
			ActionEvent: dto.IDENTITY_ADDED,
			Id:          id.Identity,
		})
	} else {
		log.Debugf("%s[%s] is already loaded", id.Name, id.FingerPrint)

		id.CId.Services.Range(func(key interface{}, value interface{}) bool {
			id.Services = append(id.Services, nil)

			val := value.(*cziti.ZService)
			var wg sync.WaitGroup
			wg.Add(1)
			rwg := &cziti.TunnelerActionWaitGroup{
				Wg:    &wg,
				Czsvc: val,
			}
			cziti.AddIntercept(rwg)
			wg.Wait()

			return true
		})

		rts.BroadcastEvent(dto.IdentityEvent{
			ActionEvent: dto.IDENTITY_CONNECTED,
			Id:          id.Identity,
		})
		log.Infof("connecting identity completed: %s[%s] %t/%t", id.Name, id.FingerPrint, id.MfaEnabled, id.MfaNeeded)
	}
}

func disconnectIdentity(id *Id) error {
	log.Infof("disconnecting identity: %s", id.Name)

	if id.Active {
		if id.CId == nil {
			return fmt.Errorf("identity has not been initialized properly. please consult the logs for details")
		} else {
			log.Debugf("ranging over services all services to remove intercept and deregister the service")

			id.CId.Services.Range(func(key interface{}, value interface{}) bool {
				val := value.(*cziti.ZService)
				var wg sync.WaitGroup
				wg.Add(1)
				rwg := &cziti.TunnelerActionWaitGroup{
					Wg:    &wg,
					Czsvc: val,
				}
				cziti.RemoveIntercept(rwg)
				wg.Wait()
				return true
			})
			rts.BroadcastEvent(dto.IdentityEvent{
				ActionEvent: dto.IDENTITY_DISCONNECTED,
				Id:          id.Identity,
			})
			log.Infof("disconnecting identity complete: %s", id.Name)
		}
	} else {
		log.Debugf("id: %s is already disconnected - not attempting to disconnected again fingerprint:%s", id.Name, id.FingerPrint)
	}

	id.Active = false
	return nil
}

func removeIdentity(out *json.Encoder, fingerprint string) {
	log.Infof("request to remove identity by fingerprint: %s", fingerprint)
	id := rts.Find(fingerprint)
	if id == nil {
		respondWithError(out, fmt.Sprintf("Could not find identity by fingerprint: %s", fingerprint), IDENTITY_NOT_FOUND, nil)
		return
	}

	anyErrs := ""
	err := disconnectIdentity(id)
	if err != nil {
		anyErrs = err.Error()
		log.Errorf("error when disconnecting identity: %s, %v", fingerprint, err)
	}

	rts.RemoveByFingerprint(fingerprint)

	//remove the file from the filesystem - first verify it's the proper file
	log.Debugf("removing identity file for fingerprint %s at %s", id.FingerPrint, id.Path())
	err = os.Remove(id.Path())
	if err != nil {
		log.Warnf("could not remove file: %s", id.Path())
	} else {
		log.Debugf("identity file removed: %s", id.Path())
	}

	//remove any ".original" file from the filesystem if there is one...
	originalFileName := id.Path() + ".original"
	_, err = os.Stat(originalFileName)
	if err == nil {
		// file does exist and no other errors. remove it.
		log.Debugf("removing .original file %s", originalFileName)
		err = os.Remove(originalFileName)
		if err != nil {
			log.Warnf("could not remove file: %s", originalFileName)
		} else {
			log.Debugf("identity file removed: %s", originalFileName)
		}
	}

	resp := dto.Response{Message: "success", Code: SUCCESS, Error: anyErrs, Payload: nil}
	respond(out, resp)
	// call shutdown some day id.CId.Shutdown()
	log.Infof("request to remove identity by fingerprint: %s responded to", fingerprint)
}

func respond(out *json.Encoder, thing interface{}) {
	//leave for debugging j := json.NewEncoder(os.Stdout)
	//leave for debugging j.Encode(thing)
	_ = out.Encode(thing)
}

func pipeName(path string) string {
	if !Debug {
		return pipeBase + path
	} else {
		return pipeBase /*+ `debug\`*/ + path
	}
}

func IpcPipeName() string {
	return pipeName("ipc")
}

func logsPipeName() string {
	return pipeName("logs")
}

func eventsPipeName() string {
	return pipeName("events")
}

func acceptServices() {
	for {
		select {
		case <-shutdown:
			return
		case bulkServiceChange := <-cziti.BulkServiceChanges:
			log.Debugf("processing a bulk service change event. Hostnames to add/remove:[%d/%d] service notifications added/removed: [%d/%d]",
				len(bulkServiceChange.HostnamesToAdd),
				len(bulkServiceChange.HostnamesToRemove),
				len(bulkServiceChange.ServicesToAdd),
				len(bulkServiceChange.ServicesToRemove))
			handleBulkServiceChange(bulkServiceChange)
		}
	}
}

func handleBulkServiceChange(sc cziti.BulkServiceChange) {
	if len(sc.HostnamesToRemove) > 0 {
		log.Debug("removing rules from NRPT")
		windns.RemoveNrptRules(sc.HostnamesToRemove)
		log.Info("removed NRPT rules for: %v", sc.HostnamesToRemove)
	} else {
		log.Debug("bulk service change had no hostnames to remove")
	}

	if len(sc.HostnamesToAdd) > 0 {
		log.Debug("adding rules to NRPT")
		windns.AddNrptRules(sc.HostnamesToAdd, rts.state.TunIpv4)
		log.Infof("mapped the following hostnames: %v", sc.HostnamesToAdd)
	}

	be := dto.BulkServiceEvent{
		ActionEvent:     dto.SERVICE_BULK,
		Fingerprint:     sc.Fingerprint,
		AddedServices:   sc.ServicesToAdd,
		RemovedServices: sc.ServicesToRemove,
	}

	rts.BroadcastEvent(be)

	id := rts.Find(sc.Fingerprint)
	if id != nil {
		var m = dto.IdentityEvent{
			ActionEvent: dto.IdentityUpdateComplete,
			Id:          Clean(id),
		}
		rts.BroadcastEvent(m)
	}

	if id != nil && !id.Notified && id.MfaEnabled {
		broadcastNotification(true)
	}

}

func addUnit(count int, unit string) (result string) {
	if (count == 1) || (count == 0) {
		result = strconv.Itoa(count) + " " + unit + " "
	} else {
		result = strconv.Itoa(count) + " " + unit + "s "
	}
	return
}

func secondsToReadableFmt(input int32) (result string) {
	seconds := input % (60 * 60 * 24)
	hours := math.Floor(float64(seconds) / 60 / 60)
	seconds = input % (60 * 60)
	minutes := math.Floor(float64(seconds) / 60)
	seconds = input % 60

	if hours > 0 {
		result = addUnit(int(hours), "hour") + addUnit(int(minutes), "minute") + addUnit(int(seconds), "second")
	} else if minutes > 0 {
		result = addUnit(int(minutes), "minute") + addUnit(int(seconds), "second")
	} else {
		result = addUnit(int(seconds), "second")
	}

	return
}

func handleEvents(isInitialized chan struct{}) {
	events.run()
	d := 5 * time.Second
	every5s := time.NewTicker(d)
	notificationFrequency = time.NewTicker(time.Duration(rts.state.NotificationFrequency) * time.Minute)

	defer log.Debugf("exiting handleEvents. loops were set for %v", d)
	<-isInitialized
	log.Info("beginning metric collection")
	for {
		select {
		case <-shutdown:
			return
		case <-every5s.C:
			s := rts.ToMetrics()

			// broadcast metrics
			rts.BroadcastEvent(dto.MetricsEvent{
				StatusEvent: dto.StatusEvent{Op: "metrics"},
				Identities:  s.Identities,
			})

		// notification message
		case <-notificationFrequency.C:
			broadcastNotification(false)
		}
	}
}

func broadcastNotification(adhoc bool) {
	changedNotifiedStatus := false
	cleanNotifications := make([]cziti.NotificationMessage, 0)
	for _, id := range rts.ids {

		if id.CId == nil || !id.CId.MfaRefreshNeeded() || !id.MfaEnabled {
			continue
		}

		if id.Notified && adhoc {
			continue
		}

		notificationMessage := ""

		var notificationMinTimeout int32 = 0
		var notificationMaxTimeout int32 = -1
		switch mfaState := id.CId.GetMFAState(int32((constants.MaximumFrequency + rts.state.NotificationFrequency) * 60)); mfaState {
		case constants.MfaAllSvcTimeout:
			notificationMessage = fmt.Sprintf("All of the services of identity %s have timed out", id.Name)
		case constants.MfaFewSvcTimeout:
			notificationMessage = fmt.Sprintf("Some of the services of identity %s have timed out", id.Name)
		case constants.MfaNearingTimeout:
			notificationMinTimeout = id.CId.GetRemainingTime(id.CId.MfaMinTimeout, id.CId.MfaMinTimeoutRem)
			notificationMessage = fmt.Sprintf("Some of the services of identity %s are timing out in %s", id.Name, secondsToReadableFmt(notificationMinTimeout))
		default:
			// do nothing
		}
		if len(notificationMessage) > 0 {
			if !id.Notified {
				rts.SetNotified(id.FingerPrint, true)
				changedNotifiedStatus = true
				log.Debugf("Generating first notification message for the identity %s - %s", id.Name, id.FingerPrint)
			}

			if id.CId.MfaMaxTimeoutRem > -1 {
				notificationMaxTimeout = id.CId.GetRemainingTime(id.CId.MfaMaxTimeout, id.CId.MfaMaxTimeoutRem)
			}
			notificationMinTimeout = id.CId.GetRemainingTime(id.CId.MfaMinTimeout, id.CId.MfaMinTimeoutRem)

			cleanNotifications = append(cleanNotifications, cziti.NotificationMessage{
				Fingerprint:       id.FingerPrint,
				IdentityName:      id.Name,
				Severity:          "major",
				MfaMinimumTimeout: notificationMinTimeout,
				MfaMaximumTimeout: notificationMaxTimeout,
				Message:           notificationMessage,
				MfaTimeDuration:   int(time.Since(id.CId.MfaLastUpdatedTime).Seconds()),
			})
		}
	}

	if changedNotifiedStatus && adhoc {
		ResetFrequency(rts.state.NotificationFrequency)
	}

	if len(cleanNotifications) > 0 {
		log.Debugf("Sending notification message to UI %v", cleanNotifications)

		rts.BroadcastEvent(cziti.TunnelNotificationEvent{
			Op:           "notification",
			Notification: cleanNotifications,
		})
	}
}

func ResetFrequency(newFrequency int) {
	notificationFrequency.Reset(time.Duration(newFrequency) * time.Minute)
}

//Removes the Config from the provided identity and returns a 'cleaned' id
func Clean(src *Id) dto.Identity {
	mfaNeeded := false
	mfaEnabled := false

	if src.CId != nil {
		mfaNeeded = src.CId.MfaNeeded
		mfaEnabled = src.CId.MfaEnabled
	}

	log.Tracef("cleaning identity: %s: mfaNeeded: %t mfaEnabled:%t", src.Name, mfaNeeded, mfaEnabled)
	AddMetrics(src)
	nid := dto.Identity{
		Name:              src.Name,
		FingerPrint:       src.FingerPrint,
		Active:            src.Active,
		Config:            idcfg.Config{},
		ControllerVersion: src.ControllerVersion,
		Status:            "",
		MfaNeeded:         mfaNeeded,
		MfaEnabled:        mfaEnabled,
		Services:          make([]*dto.Service, 0),
		Metrics:           src.Metrics,
		Tags:              nil,
	}

	if src.CId != nil {
		var mfaMinTimeoutRemaining int32 = -1
		var mfaMaxTimeoutRemaining int32 = -1
		src.CId.Services.Range(func(key interface{}, value interface{}) bool {
			//string, ZService
			val := value.(*cziti.ZService)
			var svcMinTimeoutRemaining int32 = -1

			if src.CId.MfaRefreshNeeded() && val.Service.TimeoutRemaining > -1 {
				var svcTimeout int32 = -1
				// to save the value from the pc timeout remaining, this value comes from the controller
				var svcTimeoutRemaining int32 = -1
				// fetch svc timeout and timeout remaining from pc
				for _, pc := range val.Service.PostureChecks {
					if svcTimeout == -1 || svcTimeout > int32(pc.Timeout) {
						svcTimeout = int32(pc.Timeout)
					}
					if svcTimeoutRemaining == -1 || svcTimeoutRemaining > int32(pc.TimeoutRemaining) {
						svcTimeoutRemaining = int32(pc.TimeoutRemaining)
					}
				}

				// calculate effective timeout remaining from last mfa or service update time
				svcMinTimeoutRemaining = src.CId.GetRemainingTime(svcTimeout, svcTimeoutRemaining)
				//calculate mfa min remaining timeout
				if mfaMinTimeoutRemaining == -1 && src.CId.MfaMinTimeoutRem != -1 {
					mfaMinTimeoutRemaining = src.CId.GetRemainingTime(src.CId.MfaMinTimeout, src.CId.MfaMinTimeoutRem)
				}
				//calculate mfa max remaining timeout
				if mfaMaxTimeoutRemaining == -1 && src.CId.MfaMaxTimeoutRem != -1 {
					mfaMaxTimeoutRemaining = src.CId.GetRemainingTime(src.CId.MfaMaxTimeout, src.CId.MfaMaxTimeoutRem)
				}

				atomic.StoreInt32(&val.Service.TimeoutRemaining, svcMinTimeoutRemaining)

			}

			nid.Services = append(nid.Services /*svcToDto(val)*/, val.Service)
			return true
		})
		nid.MfaMinTimeout = src.CId.MfaMinTimeout
		nid.MfaMaxTimeout = src.CId.MfaMaxTimeout
		nid.MfaMinTimeoutRem = mfaMinTimeoutRemaining
		nid.MfaMaxTimeoutRem = mfaMaxTimeoutRemaining
		nid.ServiceUpdatedTime = src.CId.ServiceUpdatedTime
		nid.MfaLastUpdatedTime = src.CId.MfaLastUpdatedTime
		if src.CId.MfaRefreshNeeded() {
			if (nid.MfaMaxTimeoutRem == 0) && nid.MfaEnabled {
				nid.MfaNeeded = true
			}
		}
	}

	nid.Config.ZtAPI = src.Config.ZtAPI
	log.Tracef("Up: %v Down %v", nid.Metrics.Up, nid.Metrics.Down)
	return nid
}

func AddMetrics(id *Id) {
	if id == nil || id.CId == nil {
		return
	}
	id.Metrics = &dto.Metrics{}
	up, down, _ := id.CId.GetMetrics()

	id.Metrics.Up = up
	id.Metrics.Down = down
}

func authMfa(out *json.Encoder, fingerprint string, code string) {
	id := rts.Find(fingerprint)
	result := cziti.AuthMFA(id.CId, code)
	if result == nil {
		// respond with auth success message immediately
		respond(out, dto.Response{Message: "AuthMFA complete", Code: SUCCESS, Error: "", Payload: fingerprint})
		rts.SetNotified(fingerprint, false)
		id.CId.UpdateMFATimeRem()
		rts.BroadcastEvent(dto.IdentityEvent{
			ActionEvent: dto.IdentityUpdateComplete,
			Id:          Clean(id),
		})

		broadcastNotification(true)
	} else {
		respondWithError(out, fmt.Sprintf("AuthMFA failed. the supplied code [%s] was not valid: %s", code, result), 1, result)
	}
}

// when the log level is updated through command line, the message is broadcasted to UI and update service as well
func sendLogLevelAndNotify(enc *json.Encoder, loglevel string) {
	rts.BroadcastEvent(dto.LogLevelEvent{
		ActionEvent: dto.LOGLEVEL_CHANGED,
		LogLevel:    loglevel,
	})
	message := fmt.Sprintf("Loglevel %s is sent to the events channel", loglevel)
	log.Info(message)
	resp := dto.Response{Message: "success", Code: SUCCESS, Error: "", Payload: message}
	respond(enc, resp)
}

// when the identity status is updated through command line, the message is sent to UI as well
func sendIdentityAndNotifyUI(enc *json.Encoder, fingerprint string) {
	for _, id := range rts.ids {
		if id.FingerPrint == fingerprint {
			rts.BroadcastEvent(dto.IdentityEvent{
				ActionEvent: dto.IDENTITY_ADDED,
				Id:          id.Identity,
			})
			message := fmt.Sprintf("Identity %s - %s updated message is sent to the events channel", id.Identity.Name, id.Identity.FingerPrint)
			log.Info(message)
			resp := dto.Response{Message: "success", Code: SUCCESS, Error: "", Payload: message}
			respond(enc, resp)
			return
		}
	}
	resp := dto.Response{Message: "", Code: ERROR, Error: "Could not find id matching fingerprint " + fingerprint, Payload: ""}
	respond(enc, resp)
}

func updateNotificationFrequency(out *json.Encoder, notificationFreq int) {

	if notificationFreq != rts.state.NotificationFrequency {
		err := rts.UpdateNotificationFrequency(notificationFreq)
		if err != nil {
			respondWithError(out, "Could not set notification frequency", UNKNOWN_ERROR, err)
			return
		}

		ResetFrequency(notificationFreq)
	}
	respond(out, dto.Response{Message: "Notification frequency is set", Code: SUCCESS, Error: "", Payload: ""})

}
