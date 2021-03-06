// Copyright (c) 2016 PaddlePaddle Authors. All Rights Reserve.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package master

import (
	"os"
	"sync"
	"time"

	"github.com/PaddlePaddle/Paddle/go/connection"
	"github.com/PaddlePaddle/recordio"
	"github.com/coreos/etcd/clientv3"
	log "github.com/sirupsen/logrus"
)

// Client is the client of the master server.
type Client struct {
	conn       *connection.Conn
	ch         chan record
	initChOnce sync.Once
}

type record struct {
	r   []byte
	err error
}

// WithBuffer sets the client to buffer the training record.
//
// bufSize is the record buffer size. NextRecord will read from this
// buffer.
func WithBuffer(bufSize int) func(*Client) error {
	return func(c *Client) error {
		if bufSize <= 0 {
			return nil
		}

		c.initChOnce.Do(func() {
			c.ch = make(chan record, bufSize)
			go c.getRecords()
		})
		return nil
	}
}

// WithAddr sets the client to use fixed master address.
func WithAddr(addr string) func(c *Client) error {
	return func(c *Client) error {
		ch := make(chan string, 1)
		ch <- addr
		go c.monitorMaster(ch)
		return nil
	}
}

// WithEtcd sets the client to use etcd for master discovery.
func WithEtcd(endpoints []string, timeout time.Duration) func(*Client) error {
	return func(c *Client) error {
		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   endpoints,
			DialTimeout: timeout,
		})
		if err != nil {
			return err
		}

		ch := make(chan string, 1)
		a, err := GetKey(cli, DefaultAddrPath, timeout)
		if err != nil {
			return err
		}

		if a != "" {
			// Master is registered, send to the master address
			// channel.
			ch <- a
		}

		go watchKey(cli, DefaultAddrPath, ch)
		go c.monitorMaster(ch)
		return nil
	}
}

// NewClient creates a new Client.
func NewClient(opts ...func(*Client) error) (*Client, error) {
	c := &Client{}
	c.conn = connection.New()

	for _, opt := range opts {
		err := opt(c)
		if err != nil {
			return nil, err
		}

	}

	return c, nil
}

func (c *Client) getRecords() {
	for {
		t, err := c.getTask()
		if err != nil {
			log.Errorf("Get task failed, sleep 3 seconds and continue, %s", err)
			time.Sleep(3 * time.Second)
			continue
		}

		for _, chunk := range t.Chunks {
			f, err := os.Open(chunk.Path)
			if err != nil {
				log.Errorln(err)
				continue
			}

			s := recordio.NewRangeScanner(f, &chunk.Index, -1, -1)
			for s.Scan() {
				c.ch <- record{s.Record(), nil}
			}

			if s.Err() != nil {
				c.ch <- record{nil, s.Err()}
				log.Errorln(err, chunk.Path)
			}

			err = f.Close()
			if err != nil {
				log.Errorln(err)
			}
		}

		// We treat a task as finished whenever the last data
		// instance of the task is read. This is not exactly
		// correct, but a reasonable approximation.
		err = c.taskFinished(t.Meta.ID)
		if err != nil {
			log.Errorln(err)
		}
	}
}

func (c *Client) monitorMaster(addrCh <-chan string) {
	lastMaster := ""
	for curMaster := range addrCh {
		// connect to the new address once address changed.
		if curMaster != lastMaster {
			if curMaster == "" {
				err := c.conn.Close()
				if err != nil {
					log.Errorln(err)
				}
			} else {
				err := c.conn.Connect(curMaster)
				if err != nil {
					log.Errorln(err)

					// connect to addr failed, set
					// to last known addr in order
					// to retry next time.
					curMaster = lastMaster
				}
			}
		}
		lastMaster = curMaster
	}
}

// SetDataset set dataset for the master server to dispatch.
//
// SetDataset can be call multiple times from different nodes. But
// only the first call will be honored.
func (c *Client) SetDataset(globPaths []string) error {
	return c.conn.Call("Service.SetDataset", globPaths, nil)
}

// getTask gets a new task from the master server.
func (c *Client) getTask() (Task, error) {
	var t Task
	err := c.conn.Call("Service.GetTask", 0, &t)
	return t, err
}

// TaskFinished tells the master server a task is finished.
func (c *Client) taskFinished(taskID int) error {
	return c.conn.Call("Service.TaskFinished", taskID, nil)
}

// TaskFailed tell the master server as task is failed.
func (c *Client) taskFailed(meta TaskMeta) error {
	return c.conn.Call("Service.TaskFailed", meta, nil)
}

// NextRecord returns next record in the dataset.
//
// NextRecord will block until the next record is available. It is
// thread-safe.
func (c *Client) NextRecord() ([]byte, error) {
	c.initChOnce.Do(func() {
		// initialize with in case WithBuffer is not used.
		c.ch = make(chan record, 0)
		go c.getRecords()
	})

	r := <-c.ch
	return r.r, r.err
}

// RequestSaveModel requests the master server to approve the caller
// to save the model.
func (c *Client) RequestSaveModel(trainerID string, blockDur time.Duration) (bool, error) {
	var need bool
	err := c.conn.Call("Service.RequestSaveModel", SaveModelRequest{TrainerID: trainerID, BlockDur: blockDur}, &need)
	return need, err
}
