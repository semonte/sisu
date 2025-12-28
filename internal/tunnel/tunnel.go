package tunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Debug controls whether tunnel operations are logged
var Debug bool

// Tunnel represents a persistent SSM session to an EC2 instance
type Tunnel struct {
	instanceID string
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	mu         sync.Mutex
	lastUsed   time.Time
	closed     bool
}

// Manager manages SSM tunnels to EC2 instances
type Manager struct {
	tunnels map[string]*Tunnel
	mu      sync.RWMutex
	profile string
	region  string
	stopCh  chan struct{}
}

// NewManager creates a new tunnel manager
func NewManager(profile, region string) *Manager {
	m := &Manager{
		tunnels: make(map[string]*Tunnel),
		profile: profile,
		region:  region,
		stopCh:  make(chan struct{}),
	}
	go m.cleanup()
	return m
}

// Get returns an existing tunnel or creates a new one
func (m *Manager) Get(ctx context.Context, instanceID string) (*Tunnel, error) {
	// Check for existing healthy tunnel
	m.mu.RLock()
	if tun, ok := m.tunnels[instanceID]; ok {
		if tun.IsAlive() {
			m.mu.RUnlock()
			return tun, nil
		}
	}
	m.mu.RUnlock()

	// Create new tunnel
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if tun, ok := m.tunnels[instanceID]; ok && tun.IsAlive() {
		return tun, nil
	}

	// Close old tunnel if exists
	if tun, ok := m.tunnels[instanceID]; ok {
		tun.Close()
		delete(m.tunnels, instanceID)
	}

	tun, err := m.create(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	m.tunnels[instanceID] = tun
	return tun, nil
}

// create starts a new SSM session with a bash relay
func (m *Manager) create(ctx context.Context, instanceID string) (*Tunnel, error) {
	// Start a regular SSM session - we'll send the relay script as the first command
	args := []string{
		"ssm", "start-session",
		"--target", instanceID,
	}
	if m.profile != "" {
		args = append(args, "--profile", m.profile)
	}
	if m.region != "" {
		args = append(args, "--region", m.region)
	}

	cmd := exec.CommandContext(ctx, "aws", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to start SSM session: %w", err)
	}

	tun := &Tunnel{
		instanceID: instanceID,
		cmd:        cmd,
		stdin:      stdin,
		stdout:     bufio.NewReader(stdout),
		lastUsed:   time.Now(),
	}

	// Wait for session to establish
	time.Sleep(2 * time.Second)

	// Start the relay loop - this becomes our command interface
	relayScript := `while IFS= read -r cmd; do eval "$cmd" 2>&1; printf '\n___SISU_END___\n'; done`
	_, err = fmt.Fprintf(stdin, "%s\n", relayScript)
	if err != nil {
		tun.Close()
		return nil, fmt.Errorf("failed to send relay script: %w", err)
	}

	// Wait for relay to start
	time.Sleep(500 * time.Millisecond)

	// Test the connection with a simple command
	output, err := tun.Execute("echo SISU_READY")
	if err != nil {
		tun.Close()
		return nil, fmt.Errorf("failed to establish SSM tunnel: %w", err)
	}

	if !strings.Contains(output, "SISU_READY") {
		tun.Close()
		return nil, fmt.Errorf("tunnel test failed, got: %s", output)
	}

	// Run a second command to ensure buffer is clean
	// This drains any leftover output from session startup
	_, _ = tun.Execute(":")

	return tun, nil
}

// Execute runs a command through the tunnel and returns the output
func (t *Tunnel) Execute(command string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return "", fmt.Errorf("tunnel is closed")
	}

	t.lastUsed = time.Now()

	// Send command with timeout
	done := make(chan struct{})
	var output string
	var err error

	go func() {
		output, err = t.executeInternal(command)
		close(done)
	}()

	select {
	case <-done:
		return output, err
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("command timed out after 30s")
	}
}

func (t *Tunnel) executeInternal(command string) (string, error) {
	// Send command
	_, err := fmt.Fprintf(t.stdin, "%s\n", command)
	if err != nil {
		return "", fmt.Errorf("failed to send command: %w", err)
	}

	// Read until delimiter
	var output strings.Builder
	for {
		line, err := t.stdout.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("failed to read output: %w", err)
		}

		// Strip ANSI escape codes and carriage returns
		cleanLine := stripANSI(line)
		cleanLine = strings.ReplaceAll(cleanLine, "\r", "")

		if strings.TrimSpace(cleanLine) == "___SISU_END___" {
			break
		}

		// Skip lines that are just the command echo or shell prompt
		trimmed := strings.TrimSpace(cleanLine)
		if trimmed == command || strings.HasPrefix(trimmed, "sh-") || trimmed == "" {
			continue
		}

		output.WriteString(cleanLine)
		if !strings.HasSuffix(cleanLine, "\n") {
			output.WriteString("\n")
		}
	}

	return output.String(), nil
}

// stripANSI removes ANSI escape codes from a string
func stripANSI(s string) string {
	var result strings.Builder
	inEscape := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			// End of escape sequence
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				inEscape = false
			}
			continue
		}
		result.WriteByte(s[i])
	}
	return result.String()
}

// ReadFile reads a file from the remote instance
func (t *Tunnel) ReadFile(remotePath string) ([]byte, error) {
	cmd := fmt.Sprintf("sudo base64 '%s' 2>/dev/null || echo 'SISU_ERROR: cannot read file'", remotePath)
	if Debug {
		fmt.Printf("DEBUG ReadFile: cmd=%q\n", cmd)
	}
	output, err := t.Execute(cmd)
	if Debug {
		fmt.Printf("DEBUG ReadFile: output=%q err=%v\n", output, err)
	}
	if err != nil {
		return nil, err
	}

	output = strings.TrimSpace(output)
	if strings.Contains(output, "SISU_ERROR:") {
		return nil, fmt.Errorf("cannot read %s", remotePath)
	}

	decoded, err := base64.StdEncoding.DecodeString(output)
	if err != nil {
		return nil, fmt.Errorf("failed to decode file content: %w", err)
	}

	if Debug {
		fmt.Printf("DEBUG ReadFile: decoded %d bytes\n", len(decoded))
	}
	return decoded, nil
}

// WriteFile writes data to a file on the remote instance
func (t *Tunnel) WriteFile(remotePath string, data []byte) error {
	const chunkSize = 64 * 1024 // 64KB chunks

	if len(data) <= chunkSize {
		// Small file: single write
		encoded := base64.StdEncoding.EncodeToString(data)
		_, err := t.Execute(fmt.Sprintf("echo '%s' | sudo base64 -d > '%s' && echo 'OK' || echo 'SISU_ERROR: cannot write'", encoded, remotePath))
		return err
	}

	// Large file: chunked write
	// Create/truncate file
	_, err := t.Execute(fmt.Sprintf("sudo sh -c \": > '%s'\" || echo 'SISU_ERROR: cannot create file'", remotePath))
	if err != nil {
		return err
	}

	// Write in chunks
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := base64.StdEncoding.EncodeToString(data[i:end])
		output, err := t.Execute(fmt.Sprintf("echo '%s' | sudo base64 -d >> '%s' && echo 'OK' || echo 'SISU_ERROR: cannot write'", chunk, remotePath))
		if err != nil {
			return err
		}
		if strings.Contains(output, "SISU_ERROR:") {
			return fmt.Errorf("failed to write chunk to %s", remotePath)
		}
	}

	return nil
}

// ListDir lists a directory on the remote instance
func (t *Tunnel) ListDir(remotePath string) (string, error) {
	output, err := t.Execute(fmt.Sprintf("sudo ls -la '%s' 2>/dev/null || echo 'SISU_ERROR: cannot access'", remotePath))
	if err != nil {
		return "", err
	}

	if strings.Contains(output, "SISU_ERROR:") {
		return "", fmt.Errorf("cannot access %s", remotePath)
	}

	return output, nil
}

// Delete removes a file from the remote instance
func (t *Tunnel) Delete(remotePath string) error {
	output, err := t.Execute(fmt.Sprintf("sudo rm '%s' 2>/dev/null && echo 'OK' || echo 'SISU_ERROR: cannot delete'", remotePath))
	if err != nil {
		return err
	}

	if strings.Contains(output, "SISU_ERROR:") {
		return fmt.Errorf("cannot delete %s", remotePath)
	}

	return nil
}

// Stat gets file info from the remote instance
// Returns: "type size perms mtime path" (mtime is seconds since epoch)
func (t *Tunnel) Stat(remotePath string) (string, error) {
	output, err := t.Execute(fmt.Sprintf("sudo stat -c '%%F %%s %%a %%Y %%n' '%s' 2>/dev/null || echo 'SISU_ERROR: not found'", remotePath))
	if err != nil {
		return "", err
	}

	if strings.Contains(output, "SISU_ERROR:") {
		return "", fmt.Errorf("path not found: %s", remotePath)
	}

	return strings.TrimSpace(output), nil
}

// IsAlive checks if the tunnel is still functional
func (t *Tunnel) IsAlive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return false
	}

	if t.cmd.ProcessState != nil && t.cmd.ProcessState.Exited() {
		return false
	}

	return true
}

// Close terminates the tunnel
func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}

	t.closed = true
	t.stdin.Close()

	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}

	return nil
}

// cleanup periodically removes idle tunnels
func (m *Manager) cleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			for id, tun := range m.tunnels {
				if time.Since(tun.lastUsed) > 5*time.Minute || !tun.IsAlive() {
					tun.Close()
					delete(m.tunnels, id)
				}
			}
			m.mu.Unlock()
		case <-m.stopCh:
			return
		}
	}
}

// Close shuts down the manager and all tunnels
func (m *Manager) Close() {
	close(m.stopCh)

	m.mu.Lock()
	defer m.mu.Unlock()

	for id, tun := range m.tunnels {
		tun.Close()
		delete(m.tunnels, id)
	}
}
