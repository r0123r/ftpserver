package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"net"
	"strings"
	"time"

	"github.com/siddontang/go/log"
)

type clientHandler struct {
	id          uint32               // ID of the client
	daddy       *FtpServer           // Server on which the connection was accepted
	driver      ClientHandlingDriver // Client handling driver
	conn        net.Conn             // TCP connection
	writer      *bufio.Writer        // Writer on the TCP connection
	reader      *bufio.Reader        // Reader on the TCP connection
	user        string               // Authenticated user
	path        string               // Current path
	command     string               // Command received on the connection
	param       string               // Param of the FTP command
	connectedAt time.Time            // Date of connection
	ctxRnfr     string               // Rename from
	ctxRest     int64                // Restart point
	debug       bool                 // Show debugging info on the server side
	transfer    transferHandler      // Transfer connection (only passive is implemented at this stage)
	transferTLS bool                 // Use TLS for transfer connection
	logger      *log.Logger          // Client handler logging
}

// newClientHandler initializes a client handler when someone connects
func (server *FtpServer) newClientHandler(connection net.Conn, id uint32) *clientHandler {

	p := &clientHandler{
		daddy:       server,
		conn:        connection,
		id:          id,
		writer:      bufio.NewWriter(connection),
		reader:      bufio.NewReader(connection),
		connectedAt: time.Now().UTC(),
		path:        "/",
		logger:      server.Logger, //"clientId", id),
	}

	// Just respecting the existing logic here, this could be probably be dropped at some point

	return p
}

func (c *clientHandler) disconnect() {
	c.conn.Close()
}

// Path provides the current working directory of the client
func (c *clientHandler) Path() string {
	return c.path
}

// SetPath changes the current working directory
func (c *clientHandler) SetPath(path string) {
	c.path = path
}

// Debug defines if we will list all interaction
func (c *clientHandler) Debug() bool {
	return c.debug
}

// SetDebug changes the debug flag
func (c *clientHandler) SetDebug(debug bool) {
	c.debug = debug
}

// ID provides the client's ID
func (c *clientHandler) ID() uint32 {
	return c.id
}

// RemoteAddr returns the remote network address.
func (c *clientHandler) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// LocalAddr returns the local network address.
func (c *clientHandler) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *clientHandler) end() {
	c.daddy.driver.UserLeft(c)
	c.daddy.clientDeparture(c)
	if c.transfer != nil {
		c.transfer.Close()
	}
}

// HandleCommands reads the stream of commands
func (c *clientHandler) HandleCommands() {
	defer c.end()

	if msg, err := c.daddy.driver.WelcomeUser(c); err == nil {
		c.writeMessage(220, msg)
	} else {
		c.writeMessage(500, msg)
		return
	}

	for {
		if c.reader == nil {
			c.logger.Debug("ftp.disconnect")
			return
		}

		// florent(2018-01-14): #58: IDLE timeout: Preparing the deadline before we read
		if c.daddy.settings.IdleTimeout > 0 {
			c.conn.SetDeadline(time.Now().Add(time.Duration(time.Second.Nanoseconds() * int64(c.daddy.settings.IdleTimeout))))
		}

		line, err := c.reader.ReadString('\n')

		if err != nil {
			// florent(2018-01-14): #58: IDLE timeout: Adding some code to deal with the deadline
			switch err := err.(type) {
			case net.Error:
				if err.Timeout() {
					// We have to extend the deadline now
					c.conn.SetDeadline(time.Now().Add(time.Minute))
					c.logger.Warn(c.id, "IDLE timeout", "err", err)
					c.writeMessage(421, fmt.Sprintf("command timeout (%d seconds): closing control connection", c.daddy.settings.IdleTimeout))
					if err := c.writer.Flush(); err != nil {
						c.logger.Error(c.id, "Network flush error", "err", err)
					}
					if err := c.conn.Close(); err != nil {
						c.logger.Error(c.id, "Network close error", "err", err)
					}
					break
				}
				c.logger.Fatal(c.id, "Network error", "err", err)
			default:
				if err == io.EOF {
					c.logger.Debug(c.id, "TCP disconnect")
				} else {
					c.logger.Error(c.id, "Read error", "err", err)
				}
			}
			return
		}

		c.logger.Debug(c.id, "<", strings.TrimRight(line, "\n"))

		c.handleCommand(line)
	}
}

// handleCommand takes care of executing the received line
func (c *clientHandler) handleCommand(line string) {
	command, param := parseLine(line)
	c.command = strings.ToUpper(command)
	c.param = param

	cmdDesc := commandsMap[c.command]
	if cmdDesc == nil {
		c.writeMessage(500, "Unknown command")
		return
	}

	if c.driver == nil && !cmdDesc.Open {
		c.writeMessage(530, "Please login with USER and PASS")
		return
	}

	// Let's prepare to recover in case there's a command error
	defer func() {
		if r := recover(); r != nil {
			c.writeMessage(500, fmt.Sprintf("Internal error: %s", r))
		}
	}()
	cmdDesc.Fn(c)
}

func (c *clientHandler) writeLine(line string) {
	c.logger.Debug(c.id, ">", line)

	c.writer.Write([]byte(line))
	c.writer.Write([]byte("\r\n"))
	c.writer.Flush()
}

func (c *clientHandler) writeMessage(code int, message string) {
	c.writeLine(fmt.Sprintf("%d %s", code, message))
}

func (c *clientHandler) TransferOpen() (net.Conn, error) {
	if c.transfer == nil {
		c.writeMessage(550, "No passive connection declared")
		return nil, errors.New("no passive connection declared")
	}
	c.writeMessage(150, "Using transfer connection")
	conn, err := c.transfer.Open()
	if err == nil {
		c.logger.Debug(c.id, "ftp.transfer_open ", "remoteAddr ", conn.RemoteAddr().String(), " localAddr ", conn.LocalAddr().String())
	}
	return conn, err
}

func (c *clientHandler) TransferClose() {
	if c.transfer != nil {
		c.writeMessage(226, "Closing transfer connection")
		c.transfer.Close()
		c.transfer = nil
	}
}

func parseLine(line string) (string, string) {
	params := strings.SplitN(strings.Trim(line, "\r\n"), " ", 2)
	if len(params) == 1 {
		return params[0], ""
	}
	return params[0], params[1]
}
