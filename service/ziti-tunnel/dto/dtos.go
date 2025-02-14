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

package dto

import (
	"log"
	"time"

	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/config"
	idcfg "github.com/openziti/sdk-golang/ziti/config"
	"github.com/openziti/sdk-golang/ziti/enroll"
)

type AddIdentity struct {
	EnrollmentFlags enroll.EnrollmentFlags `json:"Flags"`
	Id              Identity               `json:"Id"`
}

type Service struct {
	Name             string
	Id               string
	Protocols        []string
	Addresses        []Address
	Ports            []PortRange
	OwnsIntercept    bool
	PostureChecks    []PostureCheck
	IsAccessable     bool
	Timeout          int32
	TimeoutRemaining int32
}

type Address struct {
	IsHost   bool
	HostName string
	IP       string
	Prefix   int
}

type PortRange struct {
	High int
	Low  int
}

type PostureCheck struct {
	IsPassing        bool
	QueryType        string
	Id               string
	Timeout          int
	TimeoutRemaining int
}

type ServiceOwner struct {
	Network   string
	ServiceId string
}

type HostContext struct {
}

type Identity struct {
	Name               string
	FingerPrint        string
	Active             bool
	Config             idcfg.Config
	ControllerVersion  string
	Status             string
	MfaEnabled         bool
	MfaNeeded          bool
	Services           []*Service `json:",omitempty"`
	Metrics            *Metrics   `json:",omitempty"`
	Tags               []string   `json:",omitempty"`
	MfaMinTimeout      int32
	MfaMaxTimeout      int32
	MfaMinTimeoutRem   int32
	MfaMaxTimeoutRem   int32
	MfaLastUpdatedTime time.Time
	ServiceUpdatedTime time.Time
	Notified           bool
}
type Metrics struct {
	Up   int64
	Down int64
}
type CommandMsg struct {
	Function string
	Payload  map[string]interface{}
}
type Response struct {
	Code    int
	Message string
	Error   string
	Payload interface{} `json:"Payload"`
}

type TunIpInfo struct {
	Ip     string
	Subnet string
	MTU    uint16
	DNS    string
}

func (id *Identity) Path() string {
	if id.FingerPrint == "" {
		log.Fatalf("fingerprint is invalid for id %s", id.Name)
	}
	return config.Path() + id.FingerPrint + ".json"
}

type TunnelStatus struct {
	Active                bool
	Duration              int64
	Identities            []*Identity
	IpInfo                *TunIpInfo `json:"IpInfo,omitempty"`
	LogLevel              string
	ServiceVersion        ServiceVersion
	TunIpv4               string
	TunIpv4Mask           int
	Status                string
	AddDns                bool
	NotificationFrequency int
	ApiPageSize           int
}

type ServiceVersion struct {
	Version   string
	Revision  string
	BuildDate string
}

type ZitiTunnelStatus struct {
	Status  *TunnelStatus `json:",omitempty"`
	Metrics *Metrics      `json:",omitempty"`
}

type StatusEvent struct {
	Op string
}

type ActionEvent struct {
	StatusEvent
	Action string
}

type TunnelStatusEvent struct {
	StatusEvent
	Status     TunnelStatus
	ApiVersion int
}

type MetricsEvent struct {
	StatusEvent
	Identities []*Identity
}

type BulkServiceEvent struct {
	ActionEvent
	Fingerprint     string
	AddedServices   []*Service
	RemovedServices []*Service
}

type IdentityEvent struct {
	ActionEvent
	Id Identity
}

type LogLevelEvent struct {
	ActionEvent
	LogLevel string
}

type MfaEvent struct {
	ActionEvent
	Fingerprint     string
	Successful      bool
	Error           string
	ProvisioningUrl string
	RecoveryCodes   []string
}

type MfaChallenge struct {
	ActionEvent
	Fingerprint string
}

type MfaResponse struct {
}

type ControllerEvent struct {
	ActionEvent
	Fingerprint string
}
