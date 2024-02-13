package conn

import (
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/cosmos/gogoproto/proto"
	"github.com/fortytw2/leaktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tmp2p "github.com/cometbft/cometbft/api/cometbft/p2p/v1"
	pbtypes "github.com/cometbft/cometbft/api/cometbft/types/v1"
	"github.com/cometbft/cometbft/internal/protoio"
	"github.com/cometbft/cometbft/libs/log"
)

const maxPingPongPacketSize = 1024 // bytes

func createTestMConnection(conn net.Conn) *MConnection {
	onReceive := func(chID byte, msgBytes []byte) {
	}
	onError := func(r interface{}) {
	}
	c := createMConnectionWithCallbacks(conn, onReceive, onError)
	c.SetLogger(log.TestingLogger())
	return c
}

func createMConnectionWithCallbacks(
	conn net.Conn,
	onReceive func(chID byte, msgBytes []byte),
	onError func(r interface{}),
) *MConnection {
	cfg := DefaultMConnConfig()
	cfg.PingInterval = 90 * time.Millisecond
	cfg.PongTimeout = 45 * time.Millisecond
	chDescs := []*ChannelDescriptor{{ID: 0x01, Priority: 1, SendQueueCapacity: 1}}
	c := NewMConnectionWithConfig(conn, chDescs, onReceive, onError, cfg)
	c.SetLogger(log.TestingLogger())
	return c
}

func TestMConnectionSendFlushStop(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	clientConn := createTestMConnection(client)
	err := clientConn.Start()
	require.NoError(t, err)
	defer clientConn.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("abc")
	assert.True(t, clientConn.Send(0x01, msg))

	msgLength := 14

	// start the reader in a new routine, so we can flush
	errCh := make(chan error)
	go func() {
		msgB := make([]byte, msgLength)
		_, err := server.Read(msgB)
		if err != nil {
			t.Error(err)
			return
		}
		errCh <- err
	}()

	// stop the conn - it should flush all conns
	clientConn.FlushStop()

	timer := time.NewTimer(3 * time.Second)
	select {
	case <-errCh:
	case <-timer.C:
		t.Error("timed out waiting for msgs to be read")
	}
}

func TestMConnectionSend(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	mconn := createTestMConnection(client)
	err := mconn.Start()
	require.NoError(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("Ant-Man")
	assert.True(t, mconn.Send(0x01, msg))
	// Note: subsequent Send/TrySend calls could pass because we are reading from
	// the send queue in a separate goroutine.
	_, err = server.Read(make([]byte, len(msg)))
	if err != nil {
		t.Error(err)
	}
	assert.True(t, mconn.CanSend(0x01))

	msg = []byte("Spider-Man")
	assert.True(t, mconn.TrySend(0x01, msg))
	_, err = server.Read(make([]byte, len(msg)))
	if err != nil {
		t.Error(err)
	}

	assert.False(t, mconn.CanSend(0x05), "CanSend should return false because channel is unknown")
	assert.False(t, mconn.Send(0x05, []byte("Absorbing Man")), "Send should return false because channel is unknown")
}

func TestMConnectionReceive(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn1 := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn1.Start()
	require.NoError(t, err)
	defer mconn1.Stop() //nolint:errcheck // ignore for tests

	mconn2 := createTestMConnection(server)
	err = mconn2.Start()
	require.NoError(t, err)
	defer mconn2.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("Cyclops")
	assert.True(t, mconn2.Send(0x01, msg))

	select {
	case receivedBytes := <-receivedCh:
		assert.Equal(t, msg, receivedBytes)
	case err := <-errorsCh:
		t.Fatalf("Expected %s, got %+v", msg, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Did not receive %s message in 500ms", msg)
	}
}

func TestMConnectionStatus(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	mconn := createTestMConnection(client)
	err := mconn.Start()
	require.NoError(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	status := mconn.Status()
	assert.NotNil(t, status)
	assert.Zero(t, status.Channels[0].SendQueueSize)
}

// TestMConnectionPongTimeoutResultsInError verifies that an error is reported
// when a pong message is not received within the expected timeout period.
// This test simulates a scenario where a pong message is expected but not sent,
// leading to a timeout error.
func TestMConnectionPongTimeoutResultsInError(t *testing.T) {
	// Setup a server and client connection using net.Pipe for controlled communication.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Channels to capture received messages and errors.
	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})

	// Callbacks for handling received messages and errors.
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}

	// Create and start the MConnection with the provided callbacks.
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.NoError(t, err, "Starting MConnection should not produce an error.")
	defer mconn.Stop() //nolint:errcheck // Ignoring error on stop for cleanup in test context.

	// Channel to signal when a ping message is received by the server.
	serverGotPing := make(chan struct{})
	go func() {
		// Attempt to read a ping message.
		var pkt tmp2p.Packet
		_, err := protoio.NewDelimitedReader(server, maxPingPongPacketSize).ReadMsg(&pkt)
		if err != nil {
			t.Error("Reading ping message should not produce an error:", err)
			return
		}
		serverGotPing <- struct{}{}
	}()
	<-serverGotPing // Wait for the ping message to be received.

	// Calculate the expected timeout duration for receiving a pong message.
	pongTimerExpired := mconn.config.PongTimeout + 200*time.Millisecond

	// Wait for a message, an error, or the timeout period to expire.
	select {
	case msgBytes := <-receivedCh:
		t.Fatalf("Expected error due to pong timeout, but received message: %v", msgBytes)
	case err := <-errorsCh:
		if err == nil {
			t.Fatal("Expected an error due to pong timeout, but error was nil.")
		}
	case <-time.After(pongTimerExpired):
		t.Fatalf("Expected to receive an error after %v due to pong timeout.", pongTimerExpired)
	}
}

// TestMConnectionMultiplePongsInTheBeginning tests the MConnection's behavior when multiple
// pong messages are received unexpectedly at the start of the connection. This simulates
// an abuse scenario where the remote end sends pong messages without corresponding ping requests.
// TestMConnectionMultiplePongsInTheBeginning verifies the MConnection's resilience to protocol abuse,
// specifically by handling multiple unsolicited pong messages at the start of the connection.
// It ensures that the connection remains active and does not error out or close unexpectedly.
func TestMConnectionMultiplePongsInTheBeginning(t *testing.T) {
	// Establish a server-client connection using net.Pipe for controlled communication.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Channels to capture received messages and errors.
	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})

	// Callbacks for handling received messages and errors.
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}

	// Create and start the MConnection with the provided callbacks.
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.NoError(t, err, "Starting MConnection should not produce an error.")
	defer mconn.Stop() //nolint:errcheck // Ignoring error on stop for cleanup in test context.

	// Simulate sending 3 pong messages in a row to abuse the protocol.
	protoWriter := protoio.NewDelimitedWriter(server)
	for i := 0; i < 3; i++ {
		if _, err := protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{})); err != nil {
			t.Fatalf("Sending pong message should not produce an error: %v", err)
		}
	}

	// Channel to signal when a ping message is received by the server.
	serverGotPing := make(chan struct{})
	go func() {
		// Attempt to read a ping message.
		var packet tmp2p.Packet
		if _, err := protoio.NewDelimitedReader(server, maxPingPongPacketSize).ReadMsg(&packet); err != nil {
			t.Errorf("Reading ping message should not produce an error: %v", err)
			return
		}
		serverGotPing <- struct{}{}

		// Respond with a pong message.
		if _, err := protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{})); err != nil {
			t.Errorf("Sending pong message should not produce an error: %v", err)
		}
	}()
	<-serverGotPing // Wait for the ping message to be received.

	// Calculate the expected timeout duration for receiving a pong message.
	pongTimerExpired := mconn.config.PongTimeout + 20*time.Millisecond

	// Wait for a message, an error, or the timeout period to expire.
	select {
	case msgBytes := <-receivedCh:
		t.Fatalf("Expected no data, but got %v", msgBytes)
	case err := <-errorsCh:
		t.Fatalf("Expected no error, but got %v", err)
	case <-time.After(pongTimerExpired):
		// Verify that the connection is still running despite the abuse.
		assert.True(t, mconn.IsRunning(), "MConnection should still be running.")
	}
}

func TestMConnectionMultiplePings(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.NoError(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	// sending 3 pings in a row (abuse)
	// see https://github.com/tendermint/tendermint/issues/1190
	protoReader := protoio.NewDelimitedReader(server, maxPingPongPacketSize)
	protoWriter := protoio.NewDelimitedWriter(server)
	var pkt tmp2p.Packet

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPing{}))
	require.NoError(t, err)

	_, err = protoReader.ReadMsg(&pkt)
	require.NoError(t, err)

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPing{}))
	require.NoError(t, err)

	_, err = protoReader.ReadMsg(&pkt)
	require.NoError(t, err)

	_, err = protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPing{}))
	require.NoError(t, err)

	_, err = protoReader.ReadMsg(&pkt)
	require.NoError(t, err)

	assert.True(t, mconn.IsRunning())
}

// TestMConnectionPingPongs verifies the ping-pong mechanism of the MConnection.
// It ensures that ping messages are correctly responded to with pong messages and
// checks for goroutine leaks.
func TestMConnectionPingPongs(t *testing.T) {
	// Setup leak test to ensure no goroutines are leaked.
	defer leaktest.CheckTimeout(t, 10*time.Second)()

	// Establish a server-client connection using net.Pipe for controlled communication.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Channels to capture received messages and errors.
	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}

	// Create and start the MConnection with the provided callbacks.
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.NoError(t, err)
	defer mconn.Stop() //nolint:errcheck // Ignore error on stop for cleanup in test context.

	// Channel to signal when a ping message is received by the server.
	serverGotPing := make(chan struct{}, 2) // Buffer to avoid blocking the goroutine.
	go func() {
		protoReader := protoio.NewDelimitedReader(server, maxPingPongPacketSize)
		protoWriter := protoio.NewDelimitedWriter(server)
		var pkt tmp2p.PacketPing

		for i := 0; i < 2; i++ {
			// Attempt to read a ping message.
			if _, err := protoReader.ReadMsg(&pkt); err != nil {
				t.Errorf("Reading ping message should not produce an error: %v", err)
				return
			}
			serverGotPing <- struct{}{}

			// Respond with a pong message.
			if _, err := protoWriter.WriteMsg(mustWrapPacket(&tmp2p.PacketPong{})); err != nil {
				t.Errorf("Sending pong message should not produce an error: %v", err)
				return
			}
		}
	}()
	// Wait for two ping messages to be received and responded to.
	<-serverGotPing
	<-serverGotPing

	// Calculate the expected timeout duration for receiving a pong message.
	pongTimerExpired := (mconn.config.PongTimeout + 20*time.Millisecond) * 2

	// Wait for a message, an error, or the timeout period to expire.
	select {
	case msgBytes := <-receivedCh:
		t.Fatalf("Expected no data, but got %v", msgBytes)
	case err := <-errorsCh:
		t.Fatalf("Expected no error, but got %v", err)
	case <-time.After(2 * pongTimerExpired):
		// Verify that the connection is still running.
		assert.True(t, mconn.IsRunning(), "MConnection should still be running.")
	}
}

func TestMConnectionStopsAndReturnsError(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	receivedCh := make(chan []byte)
	errorsCh := make(chan interface{})
	onReceive := func(chID byte, msgBytes []byte) {
		receivedCh <- msgBytes
	}
	onError := func(r interface{}) {
		errorsCh <- r
	}
	mconn := createMConnectionWithCallbacks(client, onReceive, onError)
	err := mconn.Start()
	require.NoError(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	if err := client.Close(); err != nil {
		t.Error(err)
	}

	select {
	case receivedBytes := <-receivedCh:
		t.Fatalf("Expected error, got %v", receivedBytes)
	case err := <-errorsCh:
		assert.NotNil(t, err)
		assert.False(t, mconn.IsRunning())
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Did not receive error in 500ms")
	}
}

func newClientAndServerConnsForReadErrors(t *testing.T, chOnErr chan struct{}) (*MConnection, *MConnection) {
	t.Helper()
	server, client := NetPipe()

	onReceive := func(chID byte, msgBytes []byte) {}
	onError := func(r interface{}) {}

	// create client conn with two channels
	chDescs := []*ChannelDescriptor{
		{ID: 0x01, Priority: 1, SendQueueCapacity: 1},
		{ID: 0x02, Priority: 1, SendQueueCapacity: 1},
	}
	mconnClient := NewMConnection(client, chDescs, onReceive, onError)
	mconnClient.SetLogger(log.TestingLogger().With("module", "client"))
	err := mconnClient.Start()
	require.NoError(t, err)

	// create server conn with 1 channel
	// it fires on chOnErr when there's an error
	serverLogger := log.TestingLogger().With("module", "server")
	onError = func(r interface{}) {
		chOnErr <- struct{}{}
	}
	mconnServer := createMConnectionWithCallbacks(server, onReceive, onError)
	mconnServer.SetLogger(serverLogger)
	err = mconnServer.Start()
	require.NoError(t, err)
	return mconnClient, mconnServer
}

func expectSend(ch chan struct{}) bool {
	after := time.After(time.Second * 5)
	select {
	case <-ch:
		return true
	case <-after:
		return false
	}
}

func TestMConnectionReadErrorBadEncoding(t *testing.T) {
	chOnErr := make(chan struct{})
	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)

	client := mconnClient.conn

	// Write it.
	_, err := client.Write([]byte{1, 2, 3, 4, 5})
	require.NoError(t, err)
	assert.True(t, expectSend(chOnErr), "badly encoded msgPacket")

	t.Cleanup(func() {
		if err := mconnClient.Stop(); err != nil {
			t.Log(err)
		}
	})

	t.Cleanup(func() {
		if err := mconnServer.Stop(); err != nil {
			t.Log(err)
		}
	})
}

func TestMConnectionReadErrorUnknownChannel(t *testing.T) {
	chOnErr := make(chan struct{})
	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)

	msg := []byte("Ant-Man")

	// fail to send msg on channel unknown by client
	assert.False(t, mconnClient.Send(0x03, msg))

	// send msg on channel unknown by the server.
	// should cause an error
	assert.True(t, mconnClient.Send(0x02, msg))
	assert.True(t, expectSend(chOnErr), "unknown channel")

	t.Cleanup(func() {
		if err := mconnClient.Stop(); err != nil {
			t.Log(err)
		}
	})

	t.Cleanup(func() {
		if err := mconnServer.Stop(); err != nil {
			t.Log(err)
		}
	})
}

func TestMConnectionReadErrorLongMessage(t *testing.T) {
	chOnErr := make(chan struct{})
	chOnRcv := make(chan struct{})

	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)
	defer mconnClient.Stop() //nolint:errcheck // ignore for tests
	defer mconnServer.Stop() //nolint:errcheck // ignore for tests

	mconnServer.onReceive = func(chID byte, msgBytes []byte) {
		chOnRcv <- struct{}{}
	}

	client := mconnClient.conn
	protoWriter := protoio.NewDelimitedWriter(client)

	// send msg that's just right
	packet := tmp2p.PacketMsg{
		ChannelID: 0x01,
		EOF:       true,
		Data:      make([]byte, mconnClient.config.MaxPacketMsgPayloadSize),
	}

	_, err := protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.NoError(t, err)
	assert.True(t, expectSend(chOnRcv), "msg just right")

	// send msg that's too long
	packet = tmp2p.PacketMsg{
		ChannelID: 0x01,
		EOF:       true,
		Data:      make([]byte, mconnClient.config.MaxPacketMsgPayloadSize+100),
	}

	_, err = protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.Error(t, err)
	assert.True(t, expectSend(chOnErr), "msg too long")
}

func TestMConnectionReadErrorUnknownMsgType(t *testing.T) {
	chOnErr := make(chan struct{})
	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)
	defer mconnClient.Stop() //nolint:errcheck // ignore for tests
	defer mconnServer.Stop() //nolint:errcheck // ignore for tests

	// send msg with unknown msg type
	_, err := protoio.NewDelimitedWriter(mconnClient.conn).WriteMsg(&pbtypes.Header{ChainID: "x"})
	require.NoError(t, err)
	assert.True(t, expectSend(chOnErr), "unknown msg type")
}

func TestMConnectionTrySend(t *testing.T) {
	server, client := NetPipe()
	defer server.Close()
	defer client.Close()

	mconn := createTestMConnection(client)
	err := mconn.Start()
	require.NoError(t, err)
	defer mconn.Stop() //nolint:errcheck // ignore for tests

	msg := []byte("Semicolon-Woman")
	resultCh := make(chan string, 2)
	assert.True(t, mconn.TrySend(0x01, msg))
	_, err = server.Read(make([]byte, len(msg)))
	require.NoError(t, err)
	assert.True(t, mconn.CanSend(0x01))
	assert.True(t, mconn.TrySend(0x01, msg))
	assert.False(t, mconn.CanSend(0x01))
	go func() {
		mconn.TrySend(0x01, msg)
		resultCh <- "TrySend"
	}()
	assert.False(t, mconn.CanSend(0x01))
	assert.False(t, mconn.TrySend(0x01, msg))
	assert.Equal(t, "TrySend", <-resultCh)
}

//nolint:lll //ignore line length for tests
func TestConnVectors(t *testing.T) {
	testCases := []struct {
		testName string
		msg      proto.Message
		expBytes string
	}{
		{"PacketPing", &tmp2p.PacketPing{}, "0a00"},
		{"PacketPong", &tmp2p.PacketPong{}, "1200"},
		{"PacketMsg", &tmp2p.PacketMsg{ChannelID: 1, EOF: false, Data: []byte("data transmitted over the wire")}, "1a2208011a1e64617461207472616e736d6974746564206f766572207468652077697265"},
	}

	for _, tc := range testCases {
		tc := tc

		pm := mustWrapPacket(tc.msg)
		bz, err := pm.Marshal()
		require.NoError(t, err, tc.testName)

		require.Equal(t, tc.expBytes, hex.EncodeToString(bz), tc.testName)
	}
}

func TestMConnectionChannelOverflow(t *testing.T) {
	chOnErr := make(chan struct{})
	chOnRcv := make(chan struct{})

	mconnClient, mconnServer := newClientAndServerConnsForReadErrors(t, chOnErr)
	t.Cleanup(stopAll(t, mconnClient, mconnServer))

	mconnServer.onReceive = func(chID byte, msgBytes []byte) {
		chOnRcv <- struct{}{}
	}

	client := mconnClient.conn
	protoWriter := protoio.NewDelimitedWriter(client)

	packet := tmp2p.PacketMsg{
		ChannelID: 0x01,
		EOF:       true,
		Data:      []byte(`42`),
	}
	_, err := protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.NoError(t, err)
	assert.True(t, expectSend(chOnRcv))

	packet.ChannelID = int32(1025)
	_, err = protoWriter.WriteMsg(mustWrapPacket(&packet))
	require.NoError(t, err)
	assert.False(t, expectSend(chOnRcv))
}

type stopper interface {
	Stop() error
}

func stopAll(t *testing.T, stoppers ...stopper) func() {
	t.Helper()
	return func() {
		for _, s := range stoppers {
			if err := s.Stop(); err != nil {
				t.Log(err)
			}
		}
	}
}
