/*
Copyright 2017 Mirantis

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package stream

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/syncmap"

	"github.com/golang/glog"
)

// UnixServer listens for connections from qemu instances and sends its
// stdout to registered channels.
type UnixServer struct {
	SocketPath      string
	closeCh         chan bool
	listenDone      chan bool
	deadlineSeconds int
	UnixConnections *syncmap.Map

	outputReaders    map[string][]chan []byte
	outputReadersMux sync.Mutex

	workersWG sync.WaitGroup
}

// NewUnixServer creates new UnixServer. Requires socketPath on which it will listen
// and kubernetesDir where logs will be written
func NewUnixServer(socketPath string) *UnixServer {
	u := UnixServer{
		SocketPath:      socketPath,
		deadlineSeconds: 5,
	}
	u.UnixConnections = new(syncmap.Map)
	u.outputReaders = map[string][]chan []byte{}
	u.closeCh = make(chan bool)
	u.listenDone = make(chan bool)
	return &u
}

// Listen starts listening for connections from qemus
func (u *UnixServer) Listen() {
	glog.V(1).Info("UnixSocket Listener started")
	defer func() {
		u.listenDone <- true
	}()

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: u.SocketPath, Net: "unix"})
	if err != nil {
		glog.Error("listen error:", err)
		return
	}

	err = fixOwner(u.SocketPath)
	if err != nil {
		glog.Error("%v", err)
		return
	}
	defer func() {
		l.Close()
		u.cleanup()
	}()

	for {
		select {
		case <-u.closeCh:
			log.Println("stopping listening on", l.Addr())
			return
		default:
		}

		l.SetDeadline(time.Now().Add(time.Duration(u.deadlineSeconds) * time.Second))
		conn, err := l.AcceptUnix()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				continue
			}
			glog.Warningf("accept error:", err)
			continue
		}

		pid, err := getPidFromConnection(conn)
		if err != nil {
			glog.Warningf("couldn't get pid from connection: %v", err)
			continue
		}

		podEnv, err := getProcessEnvironment(pid)
		if err != nil {
			glog.Warningf("couldn't get pod information from pid: %v", err)
			continue
		}
		logPath := podEnv["VIRTLET_CONTAINER_LOG_PATH"]
		containerID := podEnv["VIRTLET_CONTAINER_ID"]

		oldConn, ok := u.UnixConnections.Load(containerID)
		if ok {
			glog.Warningf("closing old unix connection for vm: %s", containerID)
			go oldConn.(*net.UnixConn).Close()
		}
		u.UnixConnections.Store(containerID, conn)

		logChan := make(chan []byte)
		u.AddOutputReader(containerID, logChan)
		u.workersWG.Add(1)
		go u.reader(containerID, &u.workersWG)

		u.workersWG.Add(1)
		go NewLogWriter(logChan, logPath, &u.workersWG)
	}
}

func (u *UnixServer) reader(containerID string, wg *sync.WaitGroup) {
	defer wg.Done()
	glog.V(1).Infoln("Spawned new stream reader for container", containerID)
	connObj, ok := u.UnixConnections.Load(containerID)
	if !ok {
		glog.Error("can not load unix connection")
		return
	}
	conn := connObj.(*net.UnixConn)

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				glog.V(1).Infoln("error reading data:", err)
			}
			break
		}
		bufCopy := make([]byte, n)
		copy(bufCopy, buf)
		u.broadcast(containerID, bufCopy)
	}
	conn.Close()
	u.UnixConnections.Delete(containerID)

	// Closing all channels
	u.outputReadersMux.Lock()
	outputReaders, ok := u.outputReaders[containerID]
	if ok == false {
		outputReaders = []chan []byte{}
	}
	for _, reader := range outputReaders {
		close(reader)
	}
	delete(u.outputReaders, containerID)
	u.outputReadersMux.Unlock()

	glog.V(1).Infof("Stream reader for container '%s' stopped gracefully", containerID)
}

// Stop stops listening and waits for all writers to finish
func (u *UnixServer) Stop() {
	close(u.closeCh)
	<-u.listenDone
	u.workersWG.Wait()
	glog.V(1).Info("UnixSocket Listener stopped")
}

func (u *UnixServer) cleanup() {
	os.Remove(u.SocketPath)
	u.UnixConnections.Range(func(key, conObj interface{}) bool {
		conn := conObj.(*net.UnixConn)
		conn.Close()
		return true
	})
}

// AddOutputReader adds a new channel for containerID to send stdout
func (u *UnixServer) AddOutputReader(containerID string, newChan chan []byte) {
	u.outputReadersMux.Lock()
	defer u.outputReadersMux.Unlock()

	outputReaders, ok := u.outputReaders[containerID]
	if ok == false {
		outputReaders = []chan []byte{}
	}
	outputReaders = append(outputReaders, newChan)
	u.outputReaders[containerID] = outputReaders
}

// RemoveOutputReader removes a channel for containerID
func (u *UnixServer) RemoveOutputReader(containerID string, readerChan chan []byte) {
	u.outputReadersMux.Lock()
	defer u.outputReadersMux.Unlock()

	outputReaders, ok := u.outputReaders[containerID]
	if ok == false {
		outputReaders = []chan []byte{}
	}
	i := readerIndex(outputReaders, readerChan)
	if i != -1 {
		outputReaders = append(outputReaders[:i], outputReaders[i+1:]...)
		u.outputReaders[containerID] = outputReaders
	}
}

func (u *UnixServer) broadcast(containerID string, buf []byte) {
	u.outputReadersMux.Lock()
	outputReaders, ok := u.outputReaders[containerID]
	if ok == false {
		outputReaders = []chan []byte{}
	}
	for _, reader := range outputReaders {
		reader <- buf
	}
	u.outputReadersMux.Unlock()
}

func readerIndex(readers []chan []byte, r chan []byte) int {
	for i, v := range readers {
		if v == r {
			return i
		}
	}
	return -1
}

func fixOwner(path string) error {
	// Qemu is run as a libvirt-qemu user. It needs acces to a socket file for serial access.
	// Libvirt sets correct rights for it when vm is started but when virtlet pod restarts the file
	// is recreated with root/root set as owner/group so changing user manualy
	// FIXME: Is it better way to do it?
	user, err := user.Lookup("libvirt-qemu")
	if err != nil {
		return fmt.Errorf("Error when looking up libvirt-qemu user: %v", err)
	}
	uid, err := strconv.Atoi(user.Uid)
	if err != nil {
		return fmt.Errorf("Error when converting Uid to int: %v", err)
	}
	if err := os.Chown(path, uid, 0); err != nil {
		return fmt.Errorf("chown error: %v", err)
	}
	return nil
}
