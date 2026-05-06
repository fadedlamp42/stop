// nvim.go — introspect running neovim instances via msgpack-RPC.
//
// every nvim process auto-creates a unix listen socket at
// $TMPDIR/nvim.<user>/<random>/nvim.<pid>.0 (macOS) or under
// $XDG_RUNTIME_DIR / /tmp on linux. we discover sockets by globbing
// that directory tree — the pid is encoded in the filename, so no
// process listing is needed. nvim's own `comm` is sometimes "vim"
// (e.g. when launched as `vim --embed`), so socket-based discovery
// is more reliable than pgrep.
//
// for each socket we run `nvim --server <sock> --remote-expr <expr>`
// which evaluates an expression in the live nvim and prints the
// JSON result. the expression projects only the fields we need so
// the round-trip stays small even when buffers have many signs/marks.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NvimBuffer is one listed buffer inside a running nvim instance.
type NvimBuffer struct {
	Path       string
	IsCurrent  bool  // displayed in the active window at query time
	IsModified bool  // unsaved changes (changed != 0)
	LastUsed   int64 // unix epoch seconds; 0 means never focused this session
}

// nvimInstance is a discovered running nvim process and its server socket.
type nvimInstance struct {
	pid    int
	socket string
}

// discoverNvimSockets walks the tmpdir-rooted nvim directory and returns
// every (pid, socket) pair currently registered. uses filesystem globbing
// instead of process listing so it works regardless of how nvim was
// launched (nvim, vim --embed, neovide, etc.).
func discoverNvimSockets() []nvimInstance {
	user := os.Getenv("USER")
	if user == "" {
		return nil
	}

	// candidate roots in priority order: macOS tmpdir, linux runtime dir, /tmp.
	var roots []string
	if td := os.TempDir(); td != "" {
		roots = append(roots, filepath.Join(td, "nvim."+user))
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		roots = append(roots, filepath.Join(rt, "nvim."+user))
	}
	roots = append(roots, "/tmp/nvim."+user)

	seen := map[int]bool{}
	var out []nvimInstance
	for _, root := range roots {
		matches, err := filepath.Glob(filepath.Join(root, "*", "nvim.*.0"))
		if err != nil {
			continue
		}
		for _, m := range matches {
			pid := pidFromSocketPath(m)
			if pid <= 0 || seen[pid] {
				continue
			}
			seen[pid] = true
			out = append(out, nvimInstance{pid: pid, socket: m})
		}
	}
	return out
}

// pidFromSocketPath parses the trailing nvim.<pid>.0 filename component.
func pidFromSocketPath(path string) int {
	name := filepath.Base(path)
	// expect nvim.<pid>.0
	parts := strings.Split(name, ".")
	if len(parts) != 3 || parts[0] != "nvim" || parts[2] != "0" {
		return 0
	}
	pid, _ := strconv.Atoi(parts[1])
	return pid
}

// mapNvimToPane walks each nvim PID's ancestor chain via the process tree
// and returns nvim_pid → pane_pid for nvims that live under a tmux pane.
// nvims not under a tracked pane (e.g. launched directly from a terminal)
// are dropped.
func mapNvimToPane(instances []nvimInstance, panePIDs map[int]bool, tree map[int]int) map[int]int {
	result := make(map[int]int)
	for _, inst := range instances {
		pid := inst.pid
		for depth := 0; depth < 20; depth++ {
			if panePIDs[pid] {
				result[inst.pid] = pid
				break
			}
			ppid, ok := tree[pid]
			if !ok || ppid <= 1 {
				break
			}
			pid = ppid
		}
	}
	return result
}

// queryNvimBuffers asks a running nvim for its listed buffers and the
// currently active buffer number. the projection lambda strips heavy
// fields (signs, marks, variables) so the response stays small.
func queryNvimBuffers(socket string) ([]NvimBuffer, error) {
	if socket == "" {
		return nil, fmt.Errorf("empty socket")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	expr := `json_encode([bufnr('%'), map(getbufinfo({'buflisted':1}), {_,b -> {'bufnr':b.bufnr,'name':b.name,'changed':b.changed,'lastused':b.lastused}})])`
	out, err := exec.CommandContext(ctx, "nvim", "--server", socket, "--remote-expr", expr).Output()
	if err != nil {
		return nil, fmt.Errorf("nvim --remote-expr: %w", err)
	}

	// payload shape: [<current_bufnr>, [{bufnr,name,changed,lastused}, ...]]
	var payload []json.RawMessage
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("decoding outer: %w", err)
	}
	if len(payload) != 2 {
		return nil, fmt.Errorf("unexpected payload length %d", len(payload))
	}

	var currentBufnr int
	if err := json.Unmarshal(payload[0], &currentBufnr); err != nil {
		return nil, fmt.Errorf("decoding bufnr: %w", err)
	}

	var infos []struct {
		BufNr    int    `json:"bufnr"`
		Name     string `json:"name"`
		Changed  int    `json:"changed"`
		LastUsed int64  `json:"lastused"`
	}
	if err := json.Unmarshal(payload[1], &infos); err != nil {
		return nil, fmt.Errorf("decoding buffer info: %w", err)
	}

	var buffers []NvimBuffer
	for _, b := range infos {
		// skip [No Name] scratch buffers — nothing useful to record
		if b.Name == "" {
			continue
		}
		buffers = append(buffers, NvimBuffer{
			Path:       b.Name,
			IsCurrent:  b.BufNr == currentBufnr,
			IsModified: b.Changed != 0,
			LastUsed:   b.LastUsed,
		})
	}
	return buffers, nil
}

// collectNvimBuffers introspects every nvim instance that lives under a
// tmux pane and returns a pane_pid → buffers map. queries run in parallel
// since each spawns a subprocess and waits ~50ms for the response.
func collectNvimBuffers(panes []TmuxPane, tree map[int]int) map[int][]NvimBuffer {
	result := make(map[int][]NvimBuffer)
	if len(panes) == 0 {
		return result
	}

	panePIDs := make(map[int]bool, len(panes))
	for _, p := range panes {
		if p.PanePID > 0 {
			panePIDs[p.PanePID] = true
		}
	}

	instances := discoverNvimSockets()
	if len(instances) == 0 {
		return result
	}
	owned := mapNvimToPane(instances, panePIDs, tree)
	if len(owned) == 0 {
		return result
	}

	// invert socket lookup so the worker has everything it needs in-hand
	socketByPID := make(map[int]string, len(instances))
	for _, inst := range instances {
		socketByPID[inst.pid] = inst.socket
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for nvimPID, panePID := range owned {
		nvimPID, panePID := nvimPID, panePID
		sock := socketByPID[nvimPID]
		if sock == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			bufs, err := queryNvimBuffers(sock)
			if err != nil || len(bufs) == 0 {
				return
			}
			mu.Lock()
			// merge: a single pane could host nested nvim instances
			result[panePID] = append(result[panePID], bufs...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return result
}
