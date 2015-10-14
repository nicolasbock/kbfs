package libkbfs

import (
	"errors"
	"fmt"
	"testing"
	"time"

	keybase1 "github.com/keybase/client/go/protocol"
	rpc "github.com/keybase/go-framed-msgpack-rpc"
	"golang.org/x/net/context"
)

type unitTester struct {
	numConnects      int
	numConnectErrors int
	numDisconnects   int
	doneChan         chan bool
}

// OnConnect implements the ConnectionHandler interface.
func (ut *unitTester) OnConnect(context.Context, *Connection, keybase1.GenericClient, *rpc.Server) error {
	ut.numConnects++
	ut.doneChan <- true
	return nil
}

// OnConnectError implements the ConnectionHandler interface.
func (ut *unitTester) OnConnectError(error, time.Duration) {
	ut.numConnectErrors++
}

// OnDisconnected implements the ConnectionHandler interface.
func (ut *unitTester) OnDisconnected() {
	ut.numDisconnects++
}

// ShouldThrottle implements the ConnectionHandler interface.
func (ut *unitTester) ShouldThrottle(err error) bool {
	return err != nil && err.Error() == "throttle"
}

// Dial implements the ConnectionTransport interface.
func (ut *unitTester) Dial(ctx context.Context) (
	rpc.Transporter, error) {
	if ut.numConnectErrors == 0 {
		return nil, errors.New("intentional error to trigger reconnect")
	}
	return nil, nil
}

// IsConnected implements the ConnectionTransport interface.
func (ut *unitTester) IsConnected() bool {
	return ut.numConnects == 1
}

// Finalize implements the ConnectionTransport interface.
func (ut *unitTester) Finalize() {
}

// Close implements the ConnectionTransport interface.
func (ut *unitTester) Close() {
}

// Did the test pass?
func (ut *unitTester) Err() error {
	if ut.numConnects != 1 {
		return fmt.Errorf("expected 1 connect, got: %d", ut.numConnects)
	}
	if ut.numConnectErrors != 1 {
		return fmt.Errorf("expected 1 connect error, got: %d", ut.numConnectErrors)
	}
	if ut.numDisconnects != 0 {
		return fmt.Errorf("expected no disconnected errors, got: %d", ut.numDisconnects)
	}
	return nil
}

// Test a basic reconnect flow.
func TestReconnectBasic(t *testing.T) {
	config := NewConfigLocal()
	unitTester := &unitTester{doneChan: make(chan bool)}
	conn := newConnectionWithTransport(config, unitTester, unitTester)
	defer conn.Shutdown()
	timeout := time.After(2 * time.Second)
	select {
	case <-unitTester.doneChan:
		break
	case <-timeout:
		break
	}
	if err := unitTester.Err(); err != nil {
		t.Fatal(err)
	}
}

// Test DoCommand with throttling.
func TestDoCommandThrottle(t *testing.T) {
	config := NewConfigLocal()
	setTestLogger(config, t)
	unitTester := &unitTester{doneChan: make(chan bool)}

	throttleErr := errors.New("throttle")
	conn := newConnectionWithTransport(config, unitTester, unitTester)
	defer conn.Shutdown()
	<-unitTester.doneChan

	throttle := true
	ctx := context.Background()
	err := conn.DoCommand(ctx, func() error {
		if throttle {
			throttle = false
			return throttleErr
		}
		return nil
	})

	if err != nil {
		t.Fatal(err)
	}
}
