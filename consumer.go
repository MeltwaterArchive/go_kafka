/*
 *  Copyright (c) 2011 NeuStar, Inc.
 *  All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 *  NeuStar, the Neustar logo and related names and logos are registered
 *  trademarks, service marks or tradenames of NeuStar, Inc. All other
 *  product names, company names, marks, logos and symbols may be trademarks
 *  of their respective owners.
 */

package kafka

import (
    "encoding/binary"
    "errors"
    "io"
    "net"
    "time"

    log "github.com/Sirupsen/logrus"
)

type BrokerConsumer struct {
    broker  *Broker
    offset  uint64
    maxSize uint32
    codecs  map[byte]PayloadCodec
}

// Create a new broker consumer
// hostname - host and optionally port, delimited by ':'
// topic to consume
// partition to consume from
// offset to start consuming from
// maxSize (in bytes) of the message to consume (this should be at least as big as the biggest message to be published)
func NewBrokerConsumer(hostname string, topic string, partition int, offset uint64, maxSize uint32) *BrokerConsumer {
    return &BrokerConsumer{broker: newBroker(hostname, topic, partition),
        offset:  offset,
        maxSize: maxSize,
        codecs:  DefaultCodecsMap}
}

// Simplified consumer that defaults the offset and maxSize to 0.
// hostname - host and optionally port, delimited by ':'
// topic to consume
// partition to consume from
func NewBrokerOffsetConsumer(hostname string, topic string, partition int) *BrokerConsumer {
    return &BrokerConsumer{broker: newBroker(hostname, topic, partition),
        offset:  0,
        maxSize: 0,
        codecs:  DefaultCodecsMap}
}

// Add Custom Payload Codecs for Consumer Decoding
// payloadCodecs - an array of PayloadCodec implementations
func (consumer *BrokerConsumer) AddCodecs(payloadCodecs []PayloadCodec) {
    // merge to the default map, so one 'could' override the default codecs..
    for k, v := range codecsMap(payloadCodecs) {
        consumer.codecs[k] = v
    }
}

func (consumer *BrokerConsumer) ConsumeOnChannel(msgChan chan *Message, pollTimeoutMs int64, quit chan bool) (int, error) {
    conn, err := consumer.broker.connect()
    if err != nil {
        return -1, err
    }

    num := 0
    done := make(chan bool, 1)
    go func() {
        for {
            _, err := consumer.consumeWithConn(conn, func(msg *Message) {
                msgChan <- msg
                num += 1
            })

            // log.Printf("err: %+v", err)

            if err != nil {
                if err != io.EOF {
                    log.Println("Error reading from Kafka: ", err)
                }
                quit <- true // force quit
                log.Println("Quit message sent")
                break
            }
            time.Sleep(time.Millisecond * time.Duration(pollTimeoutMs))
        }
        log.Println("Loop Exited")
        done <- true
        log.Println("Done message sent")
    }()
    // wait to be told to stop..
    log.Println("Waiting on Quit")
    <-quit
    log.Println("Close connection")
    conn.Close()
    close(msgChan)
    log.Println("Waiting on Done")
    <-done
    log.Println("Exit consume")
    return num, err
}

type MessageHandlerFunc func(msg *Message)

func (consumer *BrokerConsumer) Consume(handlerFunc MessageHandlerFunc) (int, error) {
    conn, err := consumer.broker.connect()
    if err != nil {
        return -1, err
    }
    defer conn.Close()

    num, err := consumer.consumeWithConn(conn, handlerFunc)

    if err != nil {
        log.Println("Fatal Error: ", err)
    }

    return num, err
}

func (consumer *BrokerConsumer) consumeWithConn(conn *net.TCPConn, handlerFunc MessageHandlerFunc) (int, error) {
    _, err := conn.Write(consumer.broker.EncodeConsumeRequest(consumer.offset, consumer.maxSize))
    if err != nil {
        return -1, err
    }

    length, payload, err := consumer.broker.readResponse(conn)

    if err != nil {
        return -1, err
    }

    num := 0
    if length > 2 {
        // parse out the messages
        var currentOffset uint64 = 0
        for currentOffset <= uint64(length-4) {
            totalLength, msgs := Decode(payload[currentOffset:], consumer.codecs)
            if msgs == nil {
                // update the broker's offset for next consumption incase they want to skip this message and keep going
                consumer.offset += currentOffset
                return num, errors.New("Error Decoding Message")
            }
            msgOffset := consumer.offset + currentOffset
            for _, msg := range msgs {
                // update all of the messages offset
                // multiple messages can be at the same offset (compressed for example)
                msg.offset = msgOffset
                handlerFunc(&msg)
                num += 1
            }
            currentOffset += uint64(4 + totalLength)
        }
        // update the broker's offset for next consumption
        consumer.offset += currentOffset
    }

    return num, err
}

// Get a list of valid offsets (up to maxNumOffsets) before the given time, where
// time is in milliseconds (-1, from the latest offset available, -2 from the smallest offset available)
// The result is a list of offsets, in descending order.
func (consumer *BrokerConsumer) GetOffsets(time int64, maxNumOffsets uint32) ([]uint64, error) {
    offsets := make([]uint64, 0)

    conn, err := consumer.broker.connect()
    if err != nil {
        return offsets, err
    }

    defer conn.Close()

    _, err = conn.Write(consumer.broker.EncodeOffsetRequest(time, maxNumOffsets))
    if err != nil {
        return offsets, err
    }

    length, payload, err := consumer.broker.readResponse(conn)
    if err != nil {
        return offsets, err
    }

    if length > 4 {
        // get the number of offsets
        numOffsets := binary.BigEndian.Uint32(payload[0:])
        var currentOffset uint64 = 4
        for currentOffset < uint64(length-4) && uint32(len(offsets)) < numOffsets {
            offset := binary.BigEndian.Uint64(payload[currentOffset:])
            offsets = append(offsets, offset)
            currentOffset += 8 // offset size
        }
    }

    return offsets, err
}

// Get the current offset for a broker.
func (consumer *BrokerConsumer) GetOffset() uint64 {
    return consumer.offset
}
