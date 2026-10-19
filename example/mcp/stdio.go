package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)


// safeCloseChan 是主仓 internal/safeclose.Safe 的内联版（外部插件不能引
// internal 包）。close of closed channel 是真机 SIGTERM 实测过的 panic：
// 多个退出路径（错误/超时/重连）都可能走到同一根 channel 的 close，
// 所以除本定义外不得出现裸 close(ch)——一律走这里。
func safeCloseChan[T any](once *sync.Once, ch chan T) bool {
	if once == nil || ch == nil {
		return false
	}
	fired := false
	once.Do(func() {
		fired = true
		close(ch)
	})
	return fired
}

// StdioTransport 通过子进程 stdin/stdout 进行 JSON-RPC 通信
// pendingRPC 是待应答通道 + 它自己的 close 守卫（本仓 close 纪律）。
type pendingRPC struct {
	ch   chan *mcpRPCResponse
	once sync.Once
}

func (p *pendingRPC) closeOnce() { safeCloseChan(&p.once, p.ch) }

type StdioTransport struct {
	doneOnce sync.Once
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	mu       sync.Mutex
	pending  map[int]*pendingRPC
	done     chan struct{}
}

func NewStdioTransport(command string, args []string, env []string) (*StdioTransport, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// 忽略 stderr（MCP 服务器可能输出日志到 stderr）
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	t := &StdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		pending: make(map[int]*pendingRPC),
		done:    make(chan struct{}),
	}

	go t.readLoop()

	return t, nil
}

func (t *StdioTransport) readLoop() {
	dec := json.NewDecoder(t.stdout)
	for {
		var resp mcpRPCResponse
		if err := dec.Decode(&resp); err != nil {
			safeCloseChan(&t.doneOnce, t.done)
			// 通知所有等待的请求
			t.mu.Lock()
			for _, pr := range t.pending {
				pr.closeOnce()
			}
			t.pending = make(map[int]*pendingRPC)
			t.mu.Unlock()
			return
		}

		t.mu.Lock()
		pr, ok := t.pending[resp.ID]
		delete(t.pending, resp.ID)
		t.mu.Unlock()

		if ok {
			pr.ch <- &resp
		}
	}
}

func (t *StdioTransport) Send(req *mcpRPCRequest) (*mcpRPCResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ch := make(chan *mcpRPCResponse, 1)
	t.mu.Lock()
	t.pending[req.ID] = &pendingRPC{ch: ch}
	t.mu.Unlock()

	if _, err := t.stdin.Write(data); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write stdin: %w", err)
	}
	if _, err := t.stdin.Write([]byte("\n")); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write newline: %w", err)
	}

	// 超时保护：MCP server 进程启动慢/卡死不响应时，不能让插件加载永久阻塞
	// （stdio server 冷启动如 npx 拉包可能数十秒，给 60s）
	timeout := time.After(60 * time.Second)
	select {
	case resp := <-ch:
		return resp, nil
	case <-t.done:
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("mcp transport closed")
	case <-timeout:
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, fmt.Errorf("mcp request timeout (60s), method may be hung")
	}
}

func (t *StdioTransport) Close() error {
	t.stdin.Close()
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
	<-t.done
	return t.cmd.Wait()
}
