package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const ( // Control messages
	badNickMsg    = "badNick"
	disconnectMsg = "disconnect"
)

type signalRegistry map[string]map[string]chan bool

// A client contains the client's connection as well as an identifier and a nick
type client struct {
	connection net.Conn
	identifier string
	nickname   string

	sendBuf chan string
	recvBuf chan string

	dcSignal chan bool
}

// A chat server contains the listener as well as a list of conenctions
type chatServer struct {
	listener     net.Listener
	clients      []client
	commandGlyph string
	controlGlyph string
	commands     map[string]CommandProcessor
	logger       *log.Logger

	joinTimeout         int
	maxNickLen          int
	minNickLen          int
	msgBufSize          int
	shutdownGracePeriod int

	clientMux  sync.Mutex
	signals    signalRegistry
	threadWait sync.WaitGroup
}

// Server provides common functionality for sending messages to clients
type Server interface {
	// Send message to a list of recipients
	DistributeMessage(message string, recipients []client)

	// Send message to all participants
	Broadcast(message string)

	// RegisterCommand will register a command handler under its usable alias.
	RegisterCommand(command string, handler CommandProcessor) (err error)

	// Perform a graceful shutdown after waitTime in seconds
	Shutdown(waitTime int)

	// Open a listener
	Start(address string) error
}

func (server *chatServer) Broadcast(message string) {
	server.DistributeMessage(message, server.clients)
}

func (server *chatServer) DistributeMessage(message string, recipients []client) {
	for _, client := range recipients {
		client.connection.Write([]byte(message + "\n"))
	}
}

func (server *chatServer) RegisterCommand(command string, handler CommandProcessor) (err error) {
	if _, ok := server.commands[command]; ok {
		return alreadyRegistered{info: command}
	}

	server.commands[command] = handler
	return
}

// chatServer.Shutdown manages graceful shutdown operations
// A shutdown warning will be broadcast to all clients. If a waitTime is specified, the server will wait for that value in seconds before beginning shutdown operations.
// Once the wait period has expired, the shutdown signal will be sent.
// The method will then wait until either WaitGroup.Wait() unblocks, or the grace period expires.
func (server *chatServer) Shutdown(waitTime int) {
	done := make(chan bool, 1)

	// Closing the listener here causes Accept() to throw an error, letting chatServer.listen() exit
	server.listener.Close()

	server.logger.Infof("[SYSTEM] Shutdown called with a %d second delay", waitTime)
	if waitTime > 0 {
		server.Broadcast("[SERVER] The server will shut down in %d seconds!")
		time.Sleep(time.Duration(waitTime) * time.Second)
	}
	server.Broadcast("[SERVER] Shutting down now!")
	server.sendSignal("shutdown")

	go func(doneSignal chan bool) {
		server.threadWait.Wait()
		doneSignal <- true
	}(done)

	select {
	case <-done:
		server.logger.Info("[SERVER] All threads stoped. Shutting down.")
		return
	case <-time.After(time.Duration(server.shutdownGracePeriod) * time.Second):
		server.logger.Warn("[SERVER] Shutdown grace period expired!")
		return
	}
}

// Start will attempt to bind a TCP socket and begin listening for connections asyncronously
func (server *chatServer) Start(address string) error {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	server.listener = ln

	// Start async listening for connections
	go server.listen()

	return nil
}

// chatserver.addClient safely adds clients to the servr's client array
func (server *chatServer) addClient(newClient client) {
	server.clientMux.Lock()
	defer server.clientMux.Unlock()

	server.clients = append(server.clients, newClient)
}

func (server *chatServer) broadcastDisconnectMsg(c client, reason string) {
	server.Broadcast(fmt.Sprintf("[SERVER] %s disconnected. Reason: %s\n", c.nickname, reason))
}

// chatServer.clientConnect handles connection events
// This process includes geting the client's initial nickname, adding the client to the server's client list
// and spinning chatServer.runClient() off in a goroutine.
func (server *chatServer) clientConnect(connection net.Conn) {
	server.threadWait.Add(1)
	server.threadWait.Done()

	complete := make(chan string, 1)
	go func(done chan string) {
		nickname := ""
		for {
			nick, _ := bufio.NewReader(connection).ReadString('\n')
			if server.validateNickname(nickname) {
				nickname = nick
				break
			}
			connection.Write([]byte(server.controlGlyph + badNickMsg))
		}
		complete <- nickname
	}(complete)

	select {
	case nickname := <-complete:
		newUUID, _ := uuid.NewRandom()
		newClient := client{
			connection: connection,
			identifier: newUUID.String(),
			nickname:   nickname,

			sendBuf: make(chan string, server.msgBufSize),
			recvBuf: make(chan string, server.msgBufSize)}

		server.addClient(newClient)
		go server.runClient(newClient)
	case <-time.After(time.Duration(server.joinTimeout) * time.Second):
		server.logger.Infof("[SYSTEM] %s timed out while joining\n", connection.RemoteAddr().String())
		connection.Write([]byte(server.controlGlyph + disconnectMsg))
		connection.Close()
	}
}

// chatServer.listen hosts the Listener.Accept() method in a loop
// The function will terminate when Listener.Accept() encounters an error.
// This SHOULD mean that either the server is unable to bind the network interface OR the listener has been closed.
func (server *chatServer) listen() {
	server.threadWait.Add(1)
	defer server.threadWait.Done()

	for {
		conn, err := server.listener.Accept()
		// Handle errors
		if err != nil {
			if err, ok := err.(*net.OpError); ok {
				if strings.Contains(err.Err.Error(), "use of closed network connection") {
					break // Listener was closed, so the thread needs to stop
				}
			} else {
				server.logger.Errorf("[ERROR] Unhandled error in server listen thread! %T, %s", err, err.Err.Error())
				break
			}
		}
		server.logger.Infof("[SYSTEM] Incomming connection from %s\n", conn.RemoteAddr().String())
		go server.clientConnect(conn)
	}
	server.logger.Info("[SYSTEM] Server listen thread is stopping")
}

// chatServer.processMessage handles incomming messages from clients
// If the message begins with the server's command glyph, the method will attempt to call the appropriate handler.
// If a handler is not registered for the command, the user will be sent an error message.
// If the message does not start with a command glyph, it will be sent to all connected clients.
func (server *chatServer) processMessage(message string, source client) (ok bool, err error) {
	server.logger.Infof("%s: %s", source.nickname, message)
	if strings.HasPrefix(message, server.commandGlyph) {
		commandString := strings.Split(message, " ")
		command := strings.TrimLeft(commandString[0], server.commandGlyph)

		if handler, ok := server.commands[command]; ok {
			ok, err := handler(server, commandString, source)
			if !ok {
				distributionList := [1]client{source}
				switch err.(type) {
				case unknownCommand:
					server.DistributeMessage("[SERVER] Got unknown command "+commandString[0], distributionList[:])
				default:
					server.logger.Errorf("[SYSTEM] Encountered error of type %T", err)
					server.DistributeMessage("[SERVER] Unable to process command", distributionList[:])
				}
			}
		}
		return true, nil
	} else if strings.HasPrefix(message, server.controlGlyph) {
		server.logger.Warnf("[SYSTEM] Incomming message from %s started with control glyph!\n", source.connection.RemoteAddr().String())
		distributionList := [1]client{source}
		server.DistributeMessage("[SERVER] Unable to process message", distributionList[:])
	}
	server.Broadcast(source.nickname + ": " + message)
	return true, nil
}

// chatServer.readClient runs a client's Reader.ReadString() in a loop.
// The method exits when ReadString throws an appropriate error.
func (server *chatServer) readClient(c client, reader *bufio.Reader) {
	server.threadWait.Add(1)
	defer server.threadWait.Done()

	for {
		message, err := reader.ReadString('\n')
		// Handle connection errors
		if err != nil {
			if err, ok := err.(*net.OpError); ok {
				if err.Timeout() {
					server.logger.Infof("[SYSTEM] %s timed out\n", err.Source)
					server.broadcastDisconnectMsg(c, "connection timed out")
					break
				} else if err.Temporary() {
					if strings.Contains(err.Err.Error(), "forcibly closed") {
						server.logger.Infof("[SYSTEM] %s forcibly closed connection\n", err.Source)
						server.broadcastDisconnectMsg(c, "connection closed")
						break
					}
					server.logger.Errorf("[ERROR] Connection with %s threw a temporary error: %s\n", err.Source, err.Err.Error())
				} else {
					server.logger.Errorf("[ERROR] Connection with %s threw a generic error: %s\n", err.Source, err.Err.Error())
					server.broadcastDisconnectMsg(c, "generic error")
					break
				}
			} else {
				server.logger.Errorf("[ERROR] Got error while reading from %s: %T\n", c.connection.RemoteAddr(), err)
				server.logger.Errorf("[ERROR] Details: ", err.Error())
				break
			}
		}

		// Push message to the client's recvBuffer
		c.recvBuf <- message
	}
}

// chatServer.registerSignal safely adds a signal to the server's signal registry
func (server *chatServer) registerSignal(signalName string, signal chan bool) string {
	// If the signal name has not been registered yet, set it up
	if _, ok := server.signals[signalName]; !ok {
		server.signals[signalName] = make(map[string]chan bool)
	}

	newUUID, _ := uuid.NewRandom()
	signalID := newUUID.String()
	server.signals[signalName][signalID] = signal

	return signalID
}

// chatServer.removeSignal safely removes a signal from the server's signal registry
func (server *chatServer) removeSignal(signalName string, signalID string) {
	if _, ok := server.signals[signalName]; !ok {
		server.logger.Errorf("[SYSTEM] Could not find signal named \"%s\" to remove signal with ID %s!\n", signalName, signalID)
	} else if _, ok := server.signals[signalName][signalID]; !ok {
		server.logger.Errorf("[SYSTEM] Could not find signalID %s in signal named \"%s\" for removal!\n", signalID, signalName)
	} else {
		delete(server.signals[signalName], signalID)
	}
}

// chatServer.runClient manages a single client's connection.
// This function watches for incommming messages and terminates if the client's conenction closes or a shutdown signal is sent.
func (server *chatServer) runClient(source client) {
	server.threadWait.Add(1)

	shutdownSig := make(chan bool, 1)
	shutdownSigID := server.registerSignal("shutdown", shutdownSig)
	clientReader := bufio.NewReader(source.connection)

	defer func() {
		server.removeSignal("shutdown", shutdownSigID)
		source.connection.Close()
		defer server.threadWait.Done()
	}()

	for {
		go server.readClient(source, clientReader)
		select {
		case <-shutdownSig:
			source.connection.Write([]byte(server.controlGlyph + disconnectMsg))
			break
		case message := <-source.recvBuf:
			go server.processMessage(message, source)
		case <-source.dcSignal:
			break
		}
	}
}

// chatServer.sendSignal pushes true to all channels registered under the passed signal name
func (server *chatServer) sendSignal(signalName string) {
	if _, ok := server.signals[signalName]; !ok {
		server.logger.Errorf("[SYSTEM] Could not find signal named \"%s\" to send!\n", signalName)
	} else {
		server.logger.Infof("[SYSTEM] Sending signal to %d signals under \"%s\"\n", len(server.signals[signalName]), signalName)
		for _, signal := range server.signals[signalName] {
			signal <- true
		}
	}
}

// chatServer.validateNickname returns true if the nickname is valid
func (server *chatServer) validateNickname(nickname string) (valid bool) {
	if server.minNickLen > len(nickname) || len(nickname) > server.maxNickLen {
		return false
	}
	if strings.Contains(nickname, " ") {
		return false
	}
	if strings.HasPrefix(nickname, server.commandGlyph) {
		return false
	}
	if strings.HasPrefix(nickname, server.controlGlyph) {
		return false
	}
	return true
}

// New returns a new Server interface
func New(logger *log.Logger) Server {

	return &chatServer{
		listener:     nil,
		clients:      []client{},
		commandGlyph: "/",
		controlGlyph: "\000",
		commands:     make(map[string]CommandProcessor),
		logger:       logger,

		joinTimeout:         60,
		maxNickLen:          32,
		minNickLen:          3,
		msgBufSize:          1024,
		shutdownGracePeriod: 60,

		clientMux:  sync.Mutex{},
		signals:    make(signalRegistry),
		threadWait: sync.WaitGroup{}}
}
