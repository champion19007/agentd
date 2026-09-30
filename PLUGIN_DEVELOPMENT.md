# Agentd Plugin Development Guide

Agentd plugins allow you to monitor sources beyond standard HTTP—such as authenticated internal databases, authenticated CLI commands, Puppeteer/Playwright headless browser scrapers, or private message queues.

Plugins are executed as isolated child processes that communicate with Agentd via the **Model Context Protocol (MCP)** over `stdio` (`stdin`/`stdout`).

---

## 1. Plugin Lifecycle & Architecture

Agentd manages plugins through the `Host` adapter ([`internal/plugins/mcp.go`](internal/plugins/mcp.go)):

1. **Lazy Spawning**: Subprocesses are spawned on-demand only when a check targeting the plugin is executed.
2. **Environment Sandboxing**: Plugins do not inherit Agentd's environment. Only explicitly mapped `SecretRef` values are passed.
3. **Capability Negotiation**: Agentd handshakes with the plugin and validates that all required capabilities are advertised.
4. **Crash Isolation**: If a plugin crashes, Agentd enters exponential backoff for that plugin only, preventing process flapping without impacting other checks.
5. **Idle Reaping**: If a plugin is inactive for longer than its configured `idle_timeout` (default: 5 minutes), Agentd cleanly terminates the subprocess to reclaim system RAM.

---

## 2. MCP JSON-RPC 2.0 Wire Protocol

All communication over `stdin`/`stdout` uses JSON-RPC 2.0 delimited by newlines.

### Handshake Sequence

#### 1. Agentd sends `initialize`
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2024-11-05",
    "capabilities": {},
    "clientInfo": {
      "name": "agentd",
      "version": "1.0.0"
    }
  }
}
```

#### 2. Plugin responds with capabilities
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "protocolVersion": "2024-11-05",
    "capabilities": {
      "tools": {}
    },
    "serverInfo": {
      "name": "my-custom-plugin",
      "version": "0.1.0"
    }
  }
}
```

#### 3. Agentd acknowledges `notifications/initialized`
```json
{
  "jsonrpc": "2.0",
  "method": "notifications/initialized"
}
```

---

### Fetch Execution Sequence

#### Agentd calls the tool (`tools/call`)
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "fetch",
    "arguments": {
      "endpoint": "users/active",
      "format": "json"
    }
  }
}
```

#### Plugin responds with result
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"active_users\": 1420}"
      }
    ]
  }
}
```

---

## 3. Reference Implementation in Go

Create a standalone executable `plugin-custom`:

```go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			send(Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "go-plugin", "version": "0.1.0"},
				},
			})
		case "notifications/initialized":
			// Handshake complete; no response required for notifications
		case "tools/call":
			// Perform data fetch
			send(Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]string{
						{"type": "text", "text": `{"status": "healthy", "queue_latency_ms": 14}`},
					},
				},
			})
		}
	}
}

func send(resp Response) {
	data, _ := json.Marshal(resp)
	fmt.Println(string(data))
}
```

---

## 4. Reference Implementation in Python

```python
import sys
import json

def main():
    for line in sys.stdin:
        if not line.strip():
            continue
        req = json.loads(line)
        method = req.get("method")
        msg_id = req.get("id")

        if method == "initialize":
            resp = {
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": "py-plugin", "version": "0.1.0"}
                }
            }
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()

        elif method == "tools/call":
            resp = {
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "content": [
                        {"type": "text", "text": json.dumps({"server_uptime": 99.98})}
                    ]
                }
            }
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()

if __name__ == "__main__":
    main()
```

---

## 5. Subprocess Termination & Windows Process Tree Cleanup

When a plugin times out or is reaped due to inactivity, Agentd terminates the subprocess:
- **POSIX / Unix**: Sends `SIGKILL` to terminate the process cleanly.
- **Windows**: Child processes spawned by scripting runtimes (e.g. `python.exe`, `node.exe`, or headless browsers like `chrome.exe`) do not automatically terminate when the parent PID is killed without Windows Job Objects. Agentd immediately kills the root process and asynchronously triggers `taskkill.exe /T /F /PID <pid>` to sweep the entire process tree, preventing orphaned background worker processes.

