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

type topic struct {
	broadcast chan interface{}
	channels  map[string]chan interface{}
	done      chan bool
}

var closeConsumer = make(map[string]chan interface{}, 32)
var closeConsumerCh = make(chan string, 32)

func newTopic(cap int16) topic {
	return topic{
		broadcast: make(chan interface{}, cap),
		channels:  make(map[string]chan interface{}, cap),
		done:      make(chan bool, cap),
	}
}

func (t *topic) register(id string, c chan interface{}) {
	t.channels[id] = c
}

func (t *topic) unregister(id string) {
	c := t.channels[id]
	delete(t.channels, id)
	closeConsumer[id] = c
	clearmessages(id)
	closeConsumerCh <- id
}

func clearmessages(id string) {
	c := closeConsumer[id]
	log.Warnf("consumer has already exited the read loop, channel id:%s, Message count : %d", id, len(c))
	if len(c) > 0 {
		for i:=0; i < len(c); i++{
			<- c
		}
		log.Tracef("channel with id:%s, Message count now : %d", id, len(c))
	}
}

func (t *topic) shutdown() {
	t.done <- true
}

func (t *topic) run() {
	go func() {
		for {
			select {
			case msg := <-t.broadcast:
				for id, c := range t.channels {
					blocked := len(c) == cap(c)
					if len(c) == cap(c) {
						log.Warnf("channel with id [%s] is about to block!", id)
					}
					c <- msg
					if blocked {
						log.Tracef("channel with id [%s] message cleared!", id)
					}
				}
				break
			case consumerId := <- closeConsumerCh:
				c := closeConsumer[consumerId]
				delete(closeConsumer, consumerId)
				close(c)
				log.Tracef("channel with id:%s is closed", consumerId)
				break
			case <-t.done:
				return
			}
		}
	}()
}
