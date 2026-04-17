// Package bird provides a client for the BIRD routing daemon control socket.
package bird

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

// Client communicates with BIRD via its Unix control socket.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient creates a BIRD control socket client.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		timeout:    10 * time.Second,
	}
}

// Configure triggers a BIRD configuration reload ("birdc configure").
func (c *Client) Configure() error {
	resp, err := c.command("configure")
	if err != nil {
		return fmt.Errorf("birdc configure: %w", err)
	}
	// BIRD returns "0003 Reconfigured" on success.
	if !strings.Contains(resp, "Reconfigured") && !strings.Contains(resp, "0003") {
		return fmt.Errorf("birdc configure: unexpected response: %s", resp)
	}
	return nil
}

// ShowProtocols returns the output of "show protocols".
func (c *Client) ShowProtocols() (string, error) {
	return c.command("show protocols")
}

// ShowOSPFNeighbors returns the output of "show ospf neighbors".
func (c *Client) ShowOSPFNeighbors() (string, error) {
	return c.command("show ospf neighbors")
}

// command sends a single command to BIRD and reads the full response.
func (c *Client) command(cmd string) (string, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.timeout))

	// Read the banner line (e.g., "0001 BIRD 2.0.x ready.").
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		return "", fmt.Errorf("reading banner: %w", err)
	}

	// Send the command.
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", fmt.Errorf("sending command: %w", err)
	}

	// Read the response. BIRD uses numeric reply codes:
	// Lines starting with a 4-digit code + space are the final line.
	// Lines starting with a 4-digit code + '-' are continuation lines.
	// Lines starting with ' ' are data continuation.
	var sb strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// If we already have some output, return it.
			if sb.Len() > 0 {
				return sb.String(), nil
			}
			return "", fmt.Errorf("reading response: %w", err)
		}
		sb.WriteString(line)

		// Final reply line: 4-digit code followed by space.
		if len(line) >= 5 && line[4] == ' ' && isDigits(line[:4]) {
			break
		}
	}
	return sb.String(), nil
}

// isDigits returns true if s contains only ASCII digits.
func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
