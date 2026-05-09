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
// JSON result. one round-trip pulls buffers + windows + session
// fields so capture latency stays bounded even with rich state.

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
// fields beyond Path/IsCurrent/IsModified/LastUsed are recorded only;
// the TUI does not depend on them.
type NvimBuffer struct {
	PanePID         int
	NvimPID         int
	Path            string
	IsCurrent       bool  // displayed in the active window at query time
	IsModified      bool  // unsaved changes (changed != 0)
	LastUsed        int64 // unix epoch seconds; 0 means never focused
	CursorLine      int   // last cursor line in any window showing this buf
	LineCount       int   // total lines in the buffer
	Filetype        string
	ChangedTick     int64
	GitsignsAdded   int
	GitsignsChanged int
	GitsignsRemoved int
}

// NvimWindow is one open window inside an nvim instance. each window has
// its own cursor, viewport and bound buffer; together with NvimBuffer
// rows they reconstruct the visible editor layout.
type NvimWindow struct {
	PanePID    int
	NvimPID    int
	WinID      int
	TabNr      int
	WinNr      int
	BufNr      int
	Width      int
	Height     int
	TopLine    int
	BotLine    int
	CursorLine int
	CursorCol  int
	IsQuickfix bool
	IsLoclist  bool
	IsTerminal bool
}

// NvimSession is one-per-nvim-instance metadata: cwd, version, argv,
// and lightweight activity signals (recent : commands, search register,
// jumplist tail, quickfix/loclist sizes).
type NvimSession struct {
	PanePID      int
	NvimPID      int
	Cwd          string
	Servername   string
	NvimVersion  string   // "0.10.2"
	Argv         []string // process argv as nvim sees it
	CmdHistory   []string // last few entries from the : command history
	Search       string   // contents of @/
	Jumplist     []string // last N jump entries, formatted as "path:lnum:col"
	QuickfixSize int
	LoclistSize  int
}

// NvimCapture is the full result of one snapshot's nvim introspection.
// PerPaneBuffers preserves the original pane_pid → buffers shape used by
// the TUI/history rendering paths. Windows and Sessions are flat slices
// for direct insertion into their respective tables.
type NvimCapture struct {
	PerPaneBuffers map[int][]NvimBuffer
	Windows        []NvimWindow
	Sessions       []NvimSession
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
	parts := strings.Split(name, ".")
	if len(parts) != 3 || parts[0] != "nvim" || parts[2] != "0" {
		return 0
	}
	pid, _ := strconv.Atoi(parts[1])
	return pid
}

// mapNvimToPane walks each nvim PID's ancestor chain via the process tree
// and returns nvim_pid → pane_pid for nvims that live under a tmux pane.
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

// nvimQueryExpr is the vimscript expression evaluated on every reachable
// nvim. one round-trip returns a 4-tuple:
//   [current_bufnr, [buffers...], [windows...], session_dict]
//
// nested calls use lambdas so the entire result is built server-side and
// arrives as a single JSON document. lambda bodies must avoid line breaks
// because remote-expr is single-line.
const nvimQueryExpr = `json_encode([` +
	`bufnr('%'),` +
	`map(getbufinfo({'buflisted':1}),{_,b->{` +
	`'bufnr':b.bufnr,'name':b.name,'changed':b.changed,'changedtick':b.changedtick,` +
	`'lastused':b.lastused,'lnum':b.lnum,'linecount':b.linecount,` +
	`'filetype':getbufvar(b.bufnr,'&filetype'),` +
	`'gitsigns':getbufvar(b.bufnr,'gitsigns_status_dict','')` +
	`}}),` +
	`map(getwininfo(),{_,w->{` +
	`'winid':w.winid,'tabnr':w.tabnr,'winnr':w.winnr,'bufnr':w.bufnr,` +
	`'width':w.width,'height':w.height,'topline':w.topline,'botline':w.botline,` +
	`'quickfix':w.quickfix,'loclist':w.loclist,'terminal':w.terminal,` +
	`'cursor':nvim_win_get_cursor(w.winid)` +
	`}}),` +
	`{` +
	`'cwd':getcwd(),'servername':v:servername,'argv':v:argv,` +
	`'version':api_info().version,` +
	`'cmd_history':map(range(max([1,histnr('cmd')-4]),histnr('cmd')+1),{_,n->histget('cmd',n)}),` +
	`'search':@/,` +
	`'jumplist':getjumplist()[0][-5:],` +
	`'qf_size':len(getqflist()),'loclist_size':len(getloclist(0))` +
	`}` +
	`])`

// rawBufInfo / rawWinInfo / rawSession are the JSON shape returned by
// nvimQueryExpr. they exist only as decode targets.
type rawBufInfo struct {
	BufNr       int    `json:"bufnr"`
	Name        string `json:"name"`
	Changed     int    `json:"changed"`
	ChangedTick int64  `json:"changedtick"`
	LastUsed    int64  `json:"lastused"`
	Lnum        int    `json:"lnum"`
	LineCount   int    `json:"linecount"`
	Filetype    string `json:"filetype"`
	// gitsigns_status_dict from the gitsigns plugin if present; empty
	// string otherwise. decoded into a separate type below.
	Gitsigns json.RawMessage `json:"gitsigns"`
}

type rawWinInfo struct {
	WinID    int   `json:"winid"`
	TabNr    int   `json:"tabnr"`
	WinNr    int   `json:"winnr"`
	BufNr    int   `json:"bufnr"`
	Width    int   `json:"width"`
	Height   int   `json:"height"`
	TopLine  int   `json:"topline"`
	BotLine  int   `json:"botline"`
	Quickfix int   `json:"quickfix"`
	Loclist  int   `json:"loclist"`
	Terminal int   `json:"terminal"`
	Cursor   []int `json:"cursor"` // [row, col]
}

type rawJumpEntry struct {
	BufNr    int    `json:"bufnr"`
	Filename string `json:"filename"`
	Lnum     int    `json:"lnum"`
	Col      int    `json:"col"`
}

type rawSession struct {
	Cwd        string `json:"cwd"`
	Servername string `json:"servername"`
	Argv       []string `json:"argv"`
	Version    struct {
		Major int `json:"major"`
		Minor int `json:"minor"`
		Patch int `json:"patch"`
	} `json:"version"`
	CmdHistory   []string       `json:"cmd_history"`
	Search       string         `json:"search"`
	Jumplist     []rawJumpEntry `json:"jumplist"`
	QFSize       int            `json:"qf_size"`
	LoclistSize  int            `json:"loclist_size"`
}

type rawGitsigns struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

// queryNvimState runs nvimQueryExpr against one nvim instance and returns
// the decoded buffer/window/session triple. all paths/labels are returned
// as-is from nvim; callers add PanePID / NvimPID before persisting.
func queryNvimState(socket string, nvimPID int) ([]NvimBuffer, []NvimWindow, *NvimSession, error) {
	if socket == "" {
		return nil, nil, nil, fmt.Errorf("empty socket")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "nvim", "--server", socket, "--remote-expr", nvimQueryExpr).Output()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("nvim --remote-expr: %w", err)
	}

	var payload []json.RawMessage
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, nil, nil, fmt.Errorf("decoding outer: %w", err)
	}
	if len(payload) != 4 {
		return nil, nil, nil, fmt.Errorf("unexpected payload length %d", len(payload))
	}

	var currentBufnr int
	if err := json.Unmarshal(payload[0], &currentBufnr); err != nil {
		return nil, nil, nil, fmt.Errorf("decoding bufnr: %w", err)
	}

	var rawBufs []rawBufInfo
	if err := json.Unmarshal(payload[1], &rawBufs); err != nil {
		return nil, nil, nil, fmt.Errorf("decoding buffers: %w", err)
	}

	var rawWins []rawWinInfo
	if err := json.Unmarshal(payload[2], &rawWins); err != nil {
		return nil, nil, nil, fmt.Errorf("decoding windows: %w", err)
	}

	var rawSess rawSession
	if err := json.Unmarshal(payload[3], &rawSess); err != nil {
		return nil, nil, nil, fmt.Errorf("decoding session: %w", err)
	}

	buffers := make([]NvimBuffer, 0, len(rawBufs))
	for _, b := range rawBufs {
		// skip [No Name] scratch buffers — nothing useful to record
		if b.Name == "" {
			continue
		}
		nb := NvimBuffer{
			NvimPID:     nvimPID,
			Path:        b.Name,
			IsCurrent:   b.BufNr == currentBufnr,
			IsModified:  b.Changed != 0,
			LastUsed:    b.LastUsed,
			CursorLine:  b.Lnum,
			LineCount:   b.LineCount,
			Filetype:    b.Filetype,
			ChangedTick: b.ChangedTick,
		}
		// gitsigns_status_dict is an object when the plugin is active and
		// an empty string otherwise; only attempt object decode.
		if len(b.Gitsigns) > 0 && b.Gitsigns[0] == '{' {
			var g rawGitsigns
			if err := json.Unmarshal(b.Gitsigns, &g); err == nil {
				nb.GitsignsAdded = g.Added
				nb.GitsignsChanged = g.Changed
				nb.GitsignsRemoved = g.Removed
			}
		}
		buffers = append(buffers, nb)
	}

	windows := make([]NvimWindow, 0, len(rawWins))
	for _, w := range rawWins {
		win := NvimWindow{
			NvimPID:    nvimPID,
			WinID:      w.WinID,
			TabNr:      w.TabNr,
			WinNr:      w.WinNr,
			BufNr:      w.BufNr,
			Width:      w.Width,
			Height:     w.Height,
			TopLine:    w.TopLine,
			BotLine:    w.BotLine,
			IsQuickfix: w.Quickfix != 0,
			IsLoclist:  w.Loclist != 0,
			IsTerminal: w.Terminal != 0,
		}
		if len(w.Cursor) >= 2 {
			win.CursorLine = w.Cursor[0]
			win.CursorCol = w.Cursor[1]
		}
		windows = append(windows, win)
	}

	jumplist := make([]string, 0, len(rawSess.Jumplist))
	for _, j := range rawSess.Jumplist {
		name := j.Filename
		if name == "" {
			name = fmt.Sprintf("[buf %d]", j.BufNr)
		}
		jumplist = append(jumplist, fmt.Sprintf("%s:%d:%d", name, j.Lnum, j.Col))
	}

	session := &NvimSession{
		NvimPID:      nvimPID,
		Cwd:          rawSess.Cwd,
		Servername:   rawSess.Servername,
		NvimVersion:  fmt.Sprintf("%d.%d.%d", rawSess.Version.Major, rawSess.Version.Minor, rawSess.Version.Patch),
		Argv:         rawSess.Argv,
		CmdHistory:   filterEmpty(rawSess.CmdHistory),
		Search:       rawSess.Search,
		Jumplist:     jumplist,
		QuickfixSize: rawSess.QFSize,
		LoclistSize:  rawSess.LoclistSize,
	}
	return buffers, windows, session, nil
}

// filterEmpty drops empty strings (common for sparse history slots).
func filterEmpty(xs []string) []string {
	out := xs[:0]
	for _, s := range xs {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// collectNvimState introspects every nvim instance that lives under a
// tmux pane and returns buffers (per-pane), windows, and session rows.
// queries run in parallel since each spawns a subprocess.
func collectNvimState(panes []TmuxPane, tree map[int]int) NvimCapture {
	cap := NvimCapture{PerPaneBuffers: map[int][]NvimBuffer{}}
	if len(panes) == 0 {
		return cap
	}

	panePIDs := make(map[int]bool, len(panes))
	for _, p := range panes {
		if p.PanePID > 0 {
			panePIDs[p.PanePID] = true
		}
	}

	instances := discoverNvimSockets()
	if len(instances) == 0 {
		return cap
	}
	owned := mapNvimToPane(instances, panePIDs, tree)
	if len(owned) == 0 {
		return cap
	}

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
			bufs, wins, sess, err := queryNvimState(sock, nvimPID)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for i := range bufs {
				bufs[i].PanePID = panePID
			}
			for i := range wins {
				wins[i].PanePID = panePID
			}
			if sess != nil {
				sess.PanePID = panePID
				cap.Sessions = append(cap.Sessions, *sess)
			}
			if len(bufs) > 0 {
				// merge: a single pane could host nested nvim instances
				cap.PerPaneBuffers[panePID] = append(cap.PerPaneBuffers[panePID], bufs...)
			}
			cap.Windows = append(cap.Windows, wins...)
		}()
	}
	wg.Wait()
	return cap
}
