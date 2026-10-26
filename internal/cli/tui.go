package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/soundadam/appkit/go/tuiv2"
	"github.com/spf13/cobra"

	"github.com/soundadam/chatvault/internal/export"
	"github.com/soundadam/chatvault/internal/pipeline"
	"github.com/soundadam/chatvault/internal/vault"
)

func (app *App) newUICommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ui",
		Short:   "Browse, organise and sync in an interactive screen (the default on a terminal)",
		GroupID: groupBrowse,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.runUI(cmd.Context())
		},
	}
}

func (app *App) runUI(ctx context.Context) error {
	v, err := app.open(true)
	if err != nil {
		return err
	}
	defer v.Close()
	m := &uiModel{app: app, ctx: ctx, v: v, updater: tuiv2.New(app.Identity)}
	m.filter = textinput.New()
	m.filter.Prompt = "/ "
	m.filter.Placeholder = "name, username or folder"
	m.input = textinput.New()
	_, err = tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(app.In), tea.WithOutput(app.Out)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

type uiMode int

const (
	modeList uiMode = iota
	modeFilter
	modeChat
	modeEdit
)

type uiModel struct {
	app     *App
	ctx     context.Context
	v       *vault.Vault
	updater tuiv2.Updater

	width, height int
	mode          uiMode

	chats    []vault.Chat
	shown    []int // indexes into chats after filtering
	cursor   int
	all      bool // include hidden chats
	filter   textinput.Model
	stats    vault.Stats
	setup    setup // with no chats, what to do first
	loadErr  error
	selectID string

	// chat view
	chat   *vault.Chat
	months []vault.MonthCount
	month  int
	lines  []string // one per message or day heading
	rows   []string // lines wrapped to rowsW cells; scroll counts these
	rowsW  int
	scroll int
	files  []export.File // what the export targets wrote for the chat

	// edit prompt
	input   textinput.Model
	editFor string // "alias" or "folder"

	syncing   bool
	syncLine  string
	status    string
	statusErr bool
}

type (
	loadedMsg struct {
		chats []vault.Chat
		stats vault.Stats
		setup setup
		err   error
	}
	monthMsg struct {
		chat   string
		months []vault.MonthCount
		month  int
		lines  []string
		files  []export.File
		top    bool // show the start of the month, not the end
		err    error
	}
	revealMsg struct {
		path string
		err  error
	}
	syncEventMsg struct {
		e  pipeline.Event
		ch chan tea.Msg
	}
	syncDoneMsg struct {
		rep *pipeline.Report
		err error
	}
)

func (m *uiModel) Init() tea.Cmd { return tea.Batch(m.load(), m.updater.Init()) }

func (m *uiModel) load() tea.Cmd {
	v, ctx := m.v, m.ctx
	return func() tea.Msg {
		chats, err := v.Chats(ctx)
		if err != nil {
			return loadedMsg{err: err}
		}
		st, err := v.Stats(ctx)
		var su setup
		if len(chats) == 0 {
			su = diagnose(ctx, v)
		}
		return loadedMsg{chats: chats, stats: st, setup: su, err: err}
	}
}

func (m *uiModel) applyFilter() {
	keep := ""
	if c := m.selected(); c != nil {
		keep = c.Chat
	}
	if m.selectID != "" {
		keep, m.selectID = m.selectID, ""
	}
	q := strings.TrimSpace(m.filter.Value())
	m.shown = m.shown[:0]
	m.cursor = 0
	for i := range m.chats {
		c := &m.chats[i]
		if (c.Hidden && !m.all) || !matches(c, q) {
			continue
		}
		if c.Chat == keep {
			m.cursor = len(m.shown)
		}
		m.shown = append(m.shown, i)
	}
}

func (m *uiModel) selected() *vault.Chat {
	if m.cursor < 0 || m.cursor >= len(m.shown) {
		return nil
	}
	return &m.chats[m.shown[m.cursor]]
}

func (m *uiModel) setStatus(s string, isErr bool) { m.status, m.statusErr = s, isErr }

// loadMonth reads month i of chat c (i < 0: the latest) as display lines,
// to be shown from its start if top, else from its end.
func (m *uiModel) loadMonth(c vault.Chat, i int, top bool) tea.Cmd {
	v, ctx := m.v, m.ctx
	return func() tea.Msg {
		months, err := v.Months(ctx, c.SourceID, c.Chat)
		if err != nil || len(months) == 0 {
			return monthMsg{chat: c.Chat, err: err}
		}
		if i < 0 || i >= len(months) {
			i = len(months) - 1
		}
		names, err := v.Names(ctx, c.SourceID)
		if err != nil {
			return monthMsg{chat: c.Chat, err: err}
		}
		src, err := v.Source(ctx, c.SourceID)
		if err != nil {
			return monthMsg{chat: c.Chat, err: err}
		}
		files, err := export.ChatFiles(ctx, v, c.SourceID, c.Chat)
		if err != nil {
			return monthMsg{chat: c.Chat, err: err}
		}
		from := months[i].Month
		var lines []string
		day := ""
		err = v.Entries(ctx, c.SourceID, c.Chat, from.Unix(), from.AddDate(0, 1, 0).Unix(), func(e *vault.Entry) error {
			if d := time.Unix(e.Time, 0).Format("2006-01-02 Mon"); d != day {
				day = d
				lines = append(lines, "", styleDim.Render("── "+d+" ──"))
			}
			lines = append(lines, formatEntry(e, &c, names, src.Self)[6:])
			return nil
		})
		return monthMsg{chat: c.Chat, months: months, month: i, lines: lines, files: files, top: top, err: err}
	}
}

// exportFile picks the file to point at for month (empty: the latest one):
// the md target's if there is one, and names the other formats it is in.
func exportFile(files []export.File, month string) (f export.File, others []string, ok bool) {
	if month == "" {
		for _, x := range files {
			month = max(month, x.Month)
		}
	}
	for _, x := range files {
		if x.Month != month {
			continue
		}
		if !ok || (x.Format == "md" && f.Format != "md") {
			if ok {
				others = append(others, f.Format)
			}
			f, ok = x, true
			continue
		}
		others = append(others, x.Format)
	}
	return f, others, ok
}

// revealPath shows a file in the system's file browser; a variable so tests
// do not open windows.
var revealPath = func(p string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", "-R", p).Run()
	}
	return exec.Command("xdg-open", filepath.Dir(p)).Run()
}

// reveal shows c's export file for month (empty: the latest) in the file
// browser.
func (m *uiModel) reveal(c vault.Chat, month string) tea.Cmd {
	v, ctx := m.v, m.ctx
	return func() tea.Msg {
		files, err := export.ChatFiles(ctx, v, c.SourceID, c.Chat)
		if err != nil {
			return revealMsg{err: err}
		}
		f, _, ok := exportFile(files, month)
		if !ok {
			return revealMsg{err: errors.New("no export file for " + c.Name() + " yet: press s to sync, which writes the exports")}
		}
		return revealMsg{path: f.Path, err: revealPath(f.Path)}
	}
}

// tilde shortens a path under the home directory to ~/...
func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && filepath.IsLocal(rel) {
			return filepath.Join("~", rel)
		}
	}
	return p
}

func (m *uiModel) startSync() tea.Cmd {
	m.syncing = true
	m.syncLine = "starting"
	ch := make(chan tea.Msg, 64)
	v, ctx, version := m.v, m.ctx, m.app.Identity.Version
	go func() {
		rep, err := pipeline.Sync(ctx, v, pipeline.Options{ToolVersion: version}, func(e pipeline.Event) {
			select {
			case ch <- syncEventMsg{e: e, ch: ch}:
			default: // the screen is behind; drop progress, not results
			}
		})
		ch <- syncDoneMsg{rep: rep, err: err}
		close(ch)
	}()
	return waitSync(ch)
}

func waitSync(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		if e, ok := msg.(syncEventMsg); ok {
			e.ch = ch
			return e
		}
		return msg
	}
}

func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if m.mode != modeFilter && m.mode != modeEdit {
		m.updater, cmd = m.updater.Update(msg)
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, cmd
	case loadedMsg:
		m.loadErr = msg.err
		if msg.err == nil {
			m.chats, m.stats, m.setup = msg.chats, msg.stats, msg.setup
			m.applyFilter()
		}
		return m, cmd
	case monthMsg:
		if m.chat == nil || m.chat.Chat != msg.chat {
			return m, cmd
		}
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			return m, cmd
		}
		m.months, m.month, m.lines, m.files, m.rows = msg.months, msg.month, msg.lines, msg.files, nil
		m.scroll = 0
		if !msg.top {
			m.scroll = m.maxScroll()
		}
		return m, cmd
	case revealMsg:
		switch {
		case msg.err != nil && msg.path != "":
			m.setStatus("could not open "+tilde(msg.path)+": "+msg.err.Error(), true)
		case msg.err != nil:
			m.setStatus(msg.err.Error(), true)
		default:
			m.setStatus("shown: "+tilde(msg.path), false)
		}
		return m, cmd
	case syncEventMsg:
		e := msg.e
		m.syncLine = fmt.Sprintf("%s %d/%d %s", e.Stage, e.Done, e.Total, e.Item)
		return m, tea.Batch(cmd, waitSync(msg.ch))
	case syncDoneMsg:
		m.syncing = false
		if msg.err != nil {
			m.setStatus("sync: "+msg.err.Error(), true)
		} else if r := msg.rep; r != nil {
			m.setStatus(fmt.Sprintf("synced: %d new, %d revised, %d databases changed", r.Added, r.Revised, len(r.Changed)), false)
		}
		return m, tea.Batch(cmd, m.load())
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.mode {
		case modeFilter:
			return m.updateFilter(msg)
		case modeEdit:
			return m.updateEdit(msg)
		case modeChat:
			return m.updateChat(msg, cmd)
		}
		return m.updateList(msg, cmd)
	}
	return m, cmd
}

func (m *uiModel) updateList(msg tea.KeyPressMsg, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	page := max(1, m.listRows()-1)
	switch msg.String() {
	case "q", "esc":
		if m.filter.Value() != "" && msg.String() == "esc" {
			m.filter.SetValue("")
			m.applyFilter()
			return m, cmd
		}
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.cursor = min(len(m.shown)-1, m.cursor+1)
	case "pgup", "ctrl+b":
		m.cursor = max(0, m.cursor-page)
	case "pgdown", "ctrl+f", "space":
		m.cursor = max(0, min(len(m.shown)-1, m.cursor+page))
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = max(0, len(m.shown)-1)
	case "/":
		m.mode = modeFilter
		return m, tea.Batch(cmd, m.filter.Focus())
	case "H":
		m.all = !m.all
		m.applyFilter()
	case "enter", "right", "l":
		if c := m.selected(); c != nil {
			cc := *c
			m.chat, m.lines, m.months, m.files, m.mode = &cc, nil, nil, nil, modeChat
			return m, tea.Batch(cmd, m.loadMonth(cc, -1, false))
		}
	case "o":
		if c := m.selected(); c != nil {
			return m, tea.Batch(cmd, m.reveal(*c, ""))
		}
	case "s":
		if !m.syncing {
			m.setStatus("", false)
			return m, tea.Batch(cmd, m.startSync())
		}
	case "a", "f":
		if c := m.selected(); c != nil {
			m.editFor = map[string]string{"a": "alias", "f": "folder"}[msg.String()]
			m.input.Prompt = m.editFor + ": "
			m.input.SetValue(map[string]string{"alias": c.Alias, "folder": c.Folder}[m.editFor])
			m.input.CursorEnd()
			m.mode = modeEdit
			return m, tea.Batch(cmd, m.input.Focus())
		}
	case "h":
		if c := m.selected(); c != nil {
			hide := !c.Hidden
			if err := m.v.SetPref(m.ctx, c.SourceID, c.Chat, vault.Pref{Hidden: &hide}); err != nil {
				m.setStatus(err.Error(), true)
			} else {
				m.setStatus(map[bool]string{true: "hidden: ", false: "shown: "}[hide]+c.Name(), false)
			}
			return m, tea.Batch(cmd, m.load())
		}
	case "r":
		return m, tea.Batch(cmd, m.load())
	}
	return m, cmd
}

func (m *uiModel) updateFilter(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "esc", "down", "up":
		if msg.String() == "esc" {
			m.filter.SetValue("")
			m.applyFilter()
		}
		m.filter.Blur()
		m.mode = modeList
		return m, nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	m.applyFilter()
	return m, cmd
}

func (m *uiModel) updateEdit(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.input.Blur()
		m.mode = modeList
		return m, nil
	case "enter":
		m.input.Blur()
		m.mode = modeList
		c := m.selected()
		if c == nil {
			return m, nil
		}
		val := strings.TrimSpace(m.input.Value())
		var p vault.Pref
		if m.editFor == "alias" {
			p.Alias = &val
		} else {
			val = strings.Trim(val, "/")
			p.Folder = &val
		}
		if err := m.v.SetPref(m.ctx, c.SourceID, c.Chat, p); err != nil {
			m.setStatus(err.Error(), true)
		} else {
			m.setStatus(m.editFor+" saved for "+c.Chat+"; exports follow on the next sync", false)
		}
		m.selectID = c.Chat
		return m, m.load()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *uiModel) updateChat(msg tea.KeyPressMsg, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	rows := m.bodyRows()
	maxScroll := m.maxScroll()
	// Scrolling past either end of a month carries on into the next one.
	back := func(by int) tea.Cmd {
		if m.scroll == 0 && m.month > 0 && m.lines != nil {
			return m.loadMonth(*m.chat, m.month-1, false)
		}
		m.scroll = max(0, m.scroll-by)
		return nil
	}
	fwd := func(by int) tea.Cmd {
		if m.scroll >= maxScroll && m.month < len(m.months)-1 && m.lines != nil {
			return m.loadMonth(*m.chat, m.month+1, true)
		}
		m.scroll = min(maxScroll, m.scroll+by)
		return nil
	}
	switch msg.String() {
	case "q", "esc", "left", "backspace":
		m.mode, m.chat = modeList, nil
	case "up", "k":
		return m, tea.Batch(cmd, back(1))
	case "down", "j":
		return m, tea.Batch(cmd, fwd(1))
	case "pgup", "ctrl+b":
		return m, tea.Batch(cmd, back(rows-1))
	case "pgdown", "ctrl+f", "space":
		return m, tea.Batch(cmd, fwd(rows-1))
	case "home", "g":
		m.scroll = 0
	case "end", "G":
		m.scroll = maxScroll
	case "[", "p":
		if m.month > 0 {
			return m, tea.Batch(cmd, m.loadMonth(*m.chat, m.month-1, false))
		}
	case "]", "n":
		if m.month < len(m.months)-1 {
			return m, tea.Batch(cmd, m.loadMonth(*m.chat, m.month+1, true))
		}
	case "o":
		if len(m.months) > 0 {
			return m, tea.Batch(cmd, m.reveal(*m.chat, m.months[m.month].Month.Format("2006-01")))
		}
	}
	return m, cmd
}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCursor = lipgloss.NewStyle().Reverse(true)
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleKind   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

func (m *uiModel) View() tea.View {
	var b strings.Builder
	if m.mode == modeChat && m.chat != nil {
		m.viewChat(&b)
	} else {
		m.viewList(&b)
	}
	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

// footer is the status, sync and update lines plus key help.
func (m *uiModel) footer(help string) []string {
	var out []string
	if m.syncing {
		out = append(out, styleOK.Render("syncing ")+m.syncLine)
	}
	if m.status != "" {
		st := styleOK
		if m.statusErr {
			st = styleErr
		}
		out = append(out, st.Render(m.status))
	}
	if u := m.updater.View(); u != "" {
		out = append(out, u)
	}
	return append(out, styleDim.Render(help))
}

func (m *uiModel) listRows() int {
	return max(1, m.height-3-len(m.footer("")))
}

// bodyRows are the chat view's message rows, below the title and the
// export path.
func (m *uiModel) bodyRows() int {
	return max(1, m.height-3-len(m.footer("")))
}

// chatRows are the month's lines wrapped to the screen width.
func (m *uiModel) chatRows() []string {
	w := max(20, m.width)
	if m.rows == nil || m.rowsW != w {
		m.rows, m.rowsW = m.rows[:0], w
		for _, l := range m.lines {
			m.rows = append(m.rows, wrapRows(l, w)...)
		}
	}
	return m.rows
}

func (m *uiModel) maxScroll() int { return max(0, len(m.chatRows())-m.bodyRows()) }

// wrapRows breaks a line into rows of at most w cells, continuing under the
// text after the time, as in "15:04  who: text".
func wrapRows(l string, w int) []string {
	const indent = 7
	n := ansi.StringWidth(l)
	if n <= w {
		return []string{l}
	}
	if w < indent+10 {
		return []string{ansi.Truncate(l, w, "…")}
	}
	head, body := ansi.Cut(l, 0, indent), ansi.Cut(l, indent, n)
	out := strings.Split(ansi.Wrap(body, w-indent, ""), "\n")
	if ansi.StringWidth(out[0]) < (w-indent)/2 {
		// A long run with no spaces, as Chinese text is, went whole onto
		// the next row: break it where the row ends instead.
		out = strings.Split(ansi.Hardwrap(body, w-indent, true), "\n")
	}
	for i := range out {
		if i == 0 {
			out[i] = head + out[i]
		} else {
			out[i] = strings.Repeat(" ", indent) + out[i]
		}
	}
	return out
}

func (m *uiModel) viewList(b *strings.Builder) {
	w := max(20, m.width)
	head := fmt.Sprintf("chatvault  %d messages · %d chats", m.stats.Messages, m.stats.Chats)
	if m.stats.LastRun > 0 {
		head += " · synced " + time.Unix(m.stats.LastRun, 0).Format("01-02 15:04")
	}
	if m.all {
		head += " · showing hidden"
	}
	b.WriteString(styleTitle.Render(ansi.Truncate(head, w, "…")) + "\n")
	switch {
	case m.mode == modeEdit:
		b.WriteString(m.input.View() + "\n")
	case m.mode == modeFilter || m.filter.Value() != "":
		b.WriteString(m.filter.View() + "\n")
	default:
		b.WriteString(styleDim.Render(fmt.Sprintf("%d shown", len(m.shown))) + "\n")
	}

	rows := m.listRows()
	switch {
	case m.loadErr != nil:
		b.WriteString(styleErr.Render(m.loadErr.Error()) + "\n")
		rows--
	case len(m.chats) == 0 && !m.syncing:
		for _, l := range m.emptyLines(w) {
			b.WriteString(l + "\n")
			rows--
		}
	}
	off := 0
	if m.cursor >= rows {
		off = m.cursor - rows + 1
	}
	for i, end := off, off+rows; i < len(m.shown) && i < end; i++ {
		c := &m.chats[m.shown[i]]
		name := c.Name()
		if c.Folder != "" {
			name = c.Folder + "/" + name
		}
		if c.Hidden {
			name += " (hidden)"
		}
		right := fmt.Sprintf(" %-8s %7d  %s", c.Kind, c.Count, time.Unix(c.Last, 0).Format("2006-01-02"))
		nameW := max(4, w-ansi.StringWidth(right)-1)
		name = ansi.Truncate(name, nameW, "…")
		line := " " + name + strings.Repeat(" ", nameW-ansi.StringWidth(name)) + right
		if i == m.cursor {
			line = styleCursor.Render(line)
		} else {
			line = " " + name + strings.Repeat(" ", nameW-ansi.StringWidth(name)) + styleKind.Render(right)
		}
		b.WriteString(line + "\n")
		rows--
	}
	for ; rows > 0; rows-- {
		b.WriteString("\n")
	}
	help := "↑↓ move · enter open · o show files · / filter · a alias · f folder · h hide · H hidden · s sync · q quit"
	if m.mode == modeEdit {
		help = "enter save · esc cancel · empty clears"
	}
	for _, l := range m.footer(ansi.Truncate(help, w, "…")) {
		b.WriteString(l + "\n")
	}
}

// emptyLines say what to do when the vault has no chats yet, wrapped to w.
func (m *uiModel) emptyLines(w int) []string {
	text := "The vault is empty. " + m.setup.uiHint()
	if m.stats.Runs > 0 {
		text = "No chats yet: the last sync found no messages. Press s to sync again, or run `chatvault check`."
	}
	return strings.Split(ansi.Wrap(text, max(20, w-2), ""), "\n")
}

func (m *uiModel) viewChat(b *strings.Builder) {
	w := max(20, m.width)
	c := m.chat
	head := c.Name() + "  " + styleDim.Render(c.Chat)
	if len(m.months) > 0 {
		mc := m.months[m.month]
		head += fmt.Sprintf("  %s (%d) · month %d/%d", mc.Month.Format("2006-01"), mc.Count, m.month+1, len(m.months))
	}
	b.WriteString(styleTitle.Render(ansi.Truncate(head, w, "…")) + "\n")
	b.WriteString(styleDim.Render(m.fileLine(w)) + "\n")
	rows := m.bodyRows()
	all := m.chatRows()
	m.scroll = min(m.scroll, m.maxScroll())
	for i, end := m.scroll, m.scroll+rows; i < len(all) && i < end; i++ {
		b.WriteString(ansi.Truncate(all[i], w, "…") + "\n")
		rows--
	}
	if m.lines == nil {
		b.WriteString(styleDim.Render("loading…") + "\n")
		rows--
	}
	for ; rows > 0; rows-- {
		b.WriteString("\n")
	}
	for _, l := range m.footer(ansi.Truncate("↑↓ scroll (on into the next month) · [ ] month · o show file · esc back", w, "…")) {
		b.WriteString(l + "\n")
	}
}

// fileLine says where the shown month is exported, keeping the end of the
// path when it is too long for w.
func (m *uiModel) fileLine(w int) string {
	if len(m.months) == 0 {
		return ""
	}
	f, others, ok := exportFile(m.files, m.months[m.month].Month.Format("2006-01"))
	if !ok {
		return ansi.Truncate("not exported yet: sync writes the exports", w, "…")
	}
	tail := ""
	if len(others) > 0 {
		tail = " (also " + strings.Join(others, ", ") + ")"
	}
	p := tilde(f.Path)
	if over := ansi.StringWidth(p) + ansi.StringWidth(tail) - w; over > 0 {
		p = ansi.TruncateLeft(p, over+1, "…")
	}
	return ansi.Truncate(p+tail, w, "…")
}
