package viffui

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/text"
	"github.com/vivaui/viva/ui"

	"viff/assets/branding"
	vapp "viff/internal/app"
	"viff/internal/platform"
)

// Window is the one main window: it owns view state (selection, scroll,
// open panels) and reads everything else from the workspace.
type Window struct {
	ws  *vapp.Workspace
	inv app.Invalidator
	// deferred holds work scheduled by after(); see runDeferred.
	deferred []func()
	paints   int
	painting bool
	pal      *Palette
	theme    ui.Theme
	p        pen
	c        *ui.Canvas
	size     ui.Size
	mono     ui.MonoFace
	// native is the window the application opened; nil in headless runs.
	native *app.Window
	// quit closes the application from the status item's menu.
	quit func()

	// Registries rebuilt by every paint; event handlers read the latest.
	scrolls  []scrollRegion
	menus    map[ui.ID]func(x, y float32)
	doubles  map[ui.ID]func()
	drags    map[ui.ID]func(e dragEvent)
	keys     map[ui.ID]func(e ui.KeyEvent) bool
	lists    []listFocus
	tips     map[ui.ID]string
	editors  map[ui.ID]*text.Editor
	inputs   map[ui.ID]*app.TextInput
	areas    map[ui.ID]ui.Rect
	modalTop int // scroll regions below this index are covered by a modal

	drag      *dragState
	mouse     ui.Point
	mods      ui.Modifiers
	focusNext ui.ID
	tipID     ui.ID
	tipSince  time.Duration
	tipAt     ui.Point

	popups []*popup // menu stack: popups[0] is the root, later ones are submenus
	dialog *dialog

	// Panels.
	commitOpen bool
	gitOpen    bool
	notifyOpen bool
	commitTab  int // 0 = Commit, 1 = Stash
	gitTab     int // 0 = Log, 1 = Console, 2 = Reflog

	cm  commitView
	ed  editorView
	lg  logView
	st  stashView
	rf  reflogView
	con consoleView
	wel welcomeView
	nt  noticeView

	favPtr                map[string]*bool
	pendingRebase         string
	pendingRepositoryFile *repositoryOpenTarget
	// diagDesc is the description the user typed when a problem recording was
	// started; the diagnostics issue template repeats it.
	diagDesc string

	lastGen      uint64
	lastRoot     string
	commitDrafts map[string]string
	saveTimer    *time.Timer
	fetchStop    chan struct{}
	started      time.Duration
}

type scrollRegion struct {
	r  ui.Rect
	fn func(dx, dy float32)
}

type listFocus struct {
	scope ui.Scope
	id    ui.ID
}

// dragEvent is a press-move-release sequence on a registered drag target.
type dragEvent struct {
	Phase          int // 0 begin, 1 move, 2 end
	X, Y           float32
	StartX, StartY float32
	Mods           ui.Modifiers
}

type dragState struct {
	id     ui.ID
	fn     func(e dragEvent)
	sx, sy float32
}

const (
	dragBegin = iota
	dragMove
	dragEnd
)

// New creates the window for a workspace. The caller sets the invalidate
// hook of the workspace to w.Invalidate.
func New(ws *vapp.Workspace) *Window {
	w := &Window{ws: ws, commitOpen: true, gitOpen: true}
	ws.ConfirmCheckout = w.confirmStashCheckout
	// Only macOS currently registers the bundled face. Asking DirectWrite
	// for an uninstalled family can select a proportional fallback, breaking
	// the fixed-cell layout of diffs and the console. Use the platform's
	// actual monospace default elsewhere (Consolas on Windows).
	w.mono = ui.MonoSystem
	if runtime.GOOS == "darwin" {
		w.mono = ui.MonoFamily(platform.EditorFontFamily)
	}
	w.applyTheme()
	w.cm.init()
	w.lg.init()
	w.wel.init()
	w.st.sel = -1
	return w
}

// Invalidate requests a repaint from any goroutine.
func (w *Window) Invalidate() { w.inv.Invalidate() }

func (w *Window) applyTheme() {
	dark := true
	switch w.ws.Settings.Theme {
	case "light":
		dark = false
	case "system":
		dark = systemDark()
	}
	if dark {
		w.pal = DarkPalette()
	} else {
		w.pal = LightPalette()
	}
	w.theme = w.pal.viewTheme()
}

// SetTray hands the window the native handle the application opened and how to
// quit, which the status item's menu uses. Headless runs leave it nil.
func (w *Window) SetTray(native *app.Window, quit func()) {
	w.native = native
	w.quit = quit
}

// ShowFromTray brings the window back after a press on the status item.
func (w *Window) ShowFromTray() {
	if w.native == nil {
		return
	}
	w.native.Show()
	w.Invalidate()
}

// TrayMenu is the status item's menu: bring the window back, or quit.
func (w *Window) TrayMenu() []app.MenuItem {
	quit := w.quit
	return []app.MenuItem{
		{Title: "Show Viff", OnSelect: w.ShowFromTray},
		{Separator: true},
		{Title: "Quit Viff", OnSelect: func() {
			if quit != nil {
				quit()
			}
		}},
	}
}

// Config is the Viva window configuration.
func (w *Window) Config() app.Config {
	s := w.ws.Settings
	width, height := s.Window.Width, s.Window.Height
	if width < 900 {
		width = 1440
	}
	if height < 600 {
		height = 900
	}
	return app.Config{
		Title:          "Viff",
		Class:          "viff",
		Icon:           branding.Icon,
		Width:          width,
		Height:         height,
		MinWidth:       900,
		MinHeight:      560,
		Tree:           &tree,
		WindowMode:     app.WindowCustomTitlebar,
		TitlebarHeight: toolbarH,
		ClearColor:     app.Color{Red: float64(w.pal.Frame.Red), Green: float64(w.pal.Frame.Green), Blue: float64(w.pal.Frame.Blue), Alpha: 1},
		Paint:          w.Paint,
		OnPointer:      w.onPointer,
		OnKey:          w.onKey,
		OnShortcut:     w.onShortcut,
		OnScroll:       w.onScroll,
		OnFileDrop:     w.onFileDrop,
		OnResize:       w.onResize,
		TextInput:      w.textInput,
		Invalidator:    &w.inv,
		OnWake:         w.onWake,
		OnClosed:       w.onClosed,
	}
}

func (w *Window) onWake() {
	w.ws.Drain()
	if w.ws.ConsumeUpdatePrompt() {
		w.openUpdateDialog()
	}
	if w.ws.Update.ShouldQuit {
		w.quitForUpdate()
	}
	w.runDeferred()
	w.runDue()
}

// Start opens a repository (or shows the welcome screen for "") and starts
// background fetching.
func (w *Window) Start(dir string) {
	if dir != "" {
		w.openRepo(dir)
	}
	w.startAutoFetch()
	if w.ws.Settings.AutoUpdateCheck {
		w.ws.CheckForUpdates(false)
	}
	if w.ws.Settings.Warning != "" {
		w.ws.Notify(vapp.NoticeWarning, "Settings", w.ws.Settings.Warning)
	}
}

// ShowCrashNotice tells the user the previous run ended unexpectedly and
// offers the problem-reporting entry points. crashPath is the crash file found
// at startup (it may be empty); main calls this after Start when the
// observability crash detector reports a previous crash.
func (w *Window) ShowCrashNotice(crashPath string) {
	body := "The previous session ended unexpectedly."
	if crashPath != "" {
		body += " A crash report was saved to:\n" + crashPath
	} else {
		body += " A crash report is in the log folder."
	}
	body += "\n\nReporting the problem helps fix it."
	w.ws.Notify(vapp.NoticeWarning, "Viff closed unexpectedly last time", body,
		vapp.NoticeAction{Title: "Report a Problem…", Run: w.openReportProblem},
		vapp.NoticeAction{Title: "Open Log Folder", Run: w.openLogFolder})
}

func (w *Window) onClosed() {
	w.lg.multi.stop()
	w.flushSettings()
	w.stopAutoFetch()
	w.ws.Close()
}

// quitForUpdate exits the process after the detached update helper was
// scheduled. Viva v0.2.3's RunCanvas has no programmatic window close, so this
// repeats the onClosed teardown and lets the operating system reclaim the
// window; the helper waits for this exit before replacing files.
func (w *Window) quitForUpdate() {
	w.lg.multi.stop()
	w.flushSettings()
	w.stopAutoFetch()
	w.ws.Close()
	os.Exit(0)
}

func (w *Window) onResize(size ui.Size) {
	w.ws.Settings.Window.Width = int(size.Width)
	w.ws.Settings.Window.Height = int(size.Height)
	w.saveSettings()
}

// saveSettings writes settings shortly after the last change. Git settings
// reach the runner immediately: a command started before the write finishes
// still uses the new values.
func (w *Window) saveSettings() {
	w.ws.ApplyGitSettings()
	if w.saveTimer != nil {
		w.saveTimer.Stop()
	}
	s := w.ws.Settings
	w.saveTimer = time.AfterFunc(600*time.Millisecond, func() { _ = s.Save() })
}

func (w *Window) flushSettings() {
	w.ws.ApplyGitSettings()
	if w.saveTimer != nil {
		w.saveTimer.Stop()
	}
	_ = w.ws.Settings.Save()
}

// Paint draws the whole window.
func (w *Window) Paint(c *ui.Canvas, size ui.Size) {
	w.c, w.size = c, size
	w.painting = true
	defer func() { w.painting = false }()
	w.p = pen{c: c, pal: w.pal, mono: w.mono}
	w.scrolls = w.scrolls[:0]
	w.lists = w.lists[:0]
	w.menus = resetMap(w.menus)
	w.doubles = resetMap(w.doubles)
	w.drags = resetMap(w.drags)
	w.keys = resetMap(w.keys)
	w.tips = resetMap(w.tips)
	w.editors = resetMap(w.editors)
	w.areas = resetMap(w.areas)
	w.modalTop = 0
	if w.started == 0 {
		w.started = c.Now() + 1
	}

	full := rect(0, 0, size.Width, size.Height)
	w.p.fill(full, w.pal.Frame)
	if w.ws.Repo == nil {
		w.paintWelcome(full)
	} else {
		w.syncRepo()
		w.paintMain(full)
	}
	w.paintNotices(full)
	if w.dialog != nil {
		w.modalTop = len(w.scrolls)
		w.paintDialog(full)
	}
	if len(w.popups) > 0 {
		w.modalTop = len(w.scrolls)
		// A dialog traps the focus scope, and the input router only lets a
		// region of that scope take a press. A menu opened from the dialog's
		// form is painted after the trap is popped, so its rows carry no scope
		// and every click falls through to the dialog behind them. Paint the
		// menu inside the trapped scope so its rows stay reachable.
		scope := w.c.TrappedFocusScope()
		if !scope.IsZero() {
			w.c.PushFocusScope(scope)
		}
		w.paintPopups(full)
		if !scope.IsZero() {
			w.c.PopFocusScope()
		}
	}
	w.paintTooltip(full)
	if w.focusNext != (ui.ID{}) {
		c.RequestFocus(w.focusNext)
		next := w.focusNext
		w.after(func() {
			if w.focusNext == next {
				w.focusNext = ui.ID{}
			}
		})
	}
	w.paints++
	if len(w.deferred) > 0 {
		// Viva runs AfterPaint only in a click's event pass; any other paint
		// drops it. Deferred work therefore runs either right after the event
		// pass or on the wake this schedules, whichever comes first.
		c.AfterPaint(w.runDeferred)
		w.Invalidate()
	}
	if w.ws.Pending() {
		// Results queued before the window existed (or between wakes) are
		// applied in OnWake; ask for one.
		w.Invalidate()
	} else if w.ws.Busy() {
		c.RequestFrameIn(120 * time.Millisecond)
	}
}

func resetMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return make(map[K]V)
	}
	clear(m)
	return m
}

// syncRepo resets view state that belongs to a previous repository.
func (w *Window) syncRepo() {
	rs := w.ws.Repo
	if rs.Gen == w.lastGen {
		return
	}
	w.lastGen = rs.Gen
	w.ws.Settings.AddRecent(rs.Repo.Root)
	w.saveSettings()
	if w.commitDrafts == nil {
		w.commitDrafts = make(map[string]string)
	}
	if w.lastRoot != "" {
		draft := w.cm.msg.String()
		if w.cm.amend {
			draft = w.cm.saved
		}
		w.commitDrafts[w.lastRoot] = draft
	}
	w.lastRoot = rs.Repo.Root
	w.cm.reset()
	setText(w.cm.msg, w.commitDrafts[w.lastRoot])
	w.ed.reset()
	w.lg.reset()
	w.st.sel = -1
	w.rf = reflogView{}
	w.popups = nil
	w.dialog = nil
}

// ---- registries ----------------------------------------------------------

func (w *Window) onScrollIn(r ui.Rect, fn func(dx, dy float32)) {
	w.scrolls = append(w.scrolls, scrollRegion{r: r, fn: fn})
}

func (w *Window) onContext(id ui.ID, fn func(x, y float32)) { w.menus[id] = fn }
func (w *Window) onDouble(id ui.ID, fn func())              { w.doubles[id] = fn }
func (w *Window) onDrag(id ui.ID, fn func(e dragEvent))     { w.drags[id] = fn }
func (w *Window) onKeys(id ui.ID, fn func(e ui.KeyEvent) bool) {
	w.keys[id] = fn
}
func (w *Window) tip(id ui.ID, text string) {
	if text != "" {
		w.tips[id] = text
	}
}

// list declares a focusable list container whose rows (in scope) skip focus.
func (w *Window) list(id ui.ID, scope ui.Scope, r ui.Rect, label string) {
	w.c.HitWithOptions(id, r, label, ui.HitOptions{Role: ui.RoleListBox})
	w.lists = append(w.lists, listFocus{scope: scope, id: id})
	w.areas[id] = r
}

func (w *Window) focused(id ui.ID) bool { return w.c != nil && w.c.Focused() == id }

// ---- events --------------------------------------------------------------

func (w *Window) onPointer(e ui.PointerEvent) ui.EventResult {
	w.mouse = ui.Point{X: e.X, Y: e.Y}
	if d := w.drag; d != nil {
		switch e.Kind {
		case ui.PointerMove:
			d.fn(dragEvent{Phase: dragMove, X: e.X, Y: e.Y, StartX: d.sx, StartY: d.sy, Mods: e.Modifiers})
			return ui.EventResult{Handled: true, Repaint: true}
		case ui.PointerUp, ui.PointerExit:
			if e.Kind == ui.PointerExit && e.Captured {
				return ui.EventResult{Repaint: true}
			}
			d.fn(dragEvent{Phase: dragEnd, X: e.X, Y: e.Y, StartX: d.sx, StartY: d.sy, Mods: e.Modifiers})
			w.drag = nil
			return ui.EventResult{Repaint: true}
		}
	}
	if e.Kind == ui.PointerMove {
		for _, p := range w.popups {
			p.byKey = false
		}
	}
	if e.Kind == ui.PointerMove || e.Kind == ui.PointerExit {
		if w.tipID != (ui.ID{}) {
			return ui.EventResult{Repaint: true}
		}
		return ui.EventResult{}
	}
	if e.Kind != ui.PointerDown {
		return ui.EventResult{}
	}
	w.tipID = ui.ID{}
	w.mods = e.Modifiers
	var res ui.EventResult
	for _, l := range w.lists {
		if _, ok := e.Target.In(l.scope); ok || e.Target == l.id {
			res.Focus = l.id
			break
		}
	}
	switch e.Button {
	case ui.PointerButtonPrimary:
		if fn, ok := w.drags[e.Target]; ok {
			w.drag = &dragState{id: e.Target, fn: fn, sx: e.X, sy: e.Y}
			fn(dragEvent{Phase: dragBegin, X: e.X, Y: e.Y, StartX: e.X, StartY: e.Y, Mods: e.Modifiers})
			res.Repaint = true
		}
		if e.ClickCount == 2 {
			if fn, ok := w.doubles[e.Target]; ok {
				fn()
				res.Handled, res.Repaint = true, true
			}
		}
	case ui.PointerButtonSecondary:
		if fn, ok := w.menus[e.Target]; ok {
			fn(e.X, e.Y)
			res.Handled, res.Repaint = true, true
		}
	}
	return res
}

func (w *Window) onScroll(e ui.ScrollEvent) ui.EventResult {
	dx, dy := e.DeltaX, e.DeltaY
	if e.Modifiers&ui.ModifierShift != 0 && dx == 0 {
		dx, dy = dy, 0
	}
	if !e.Precise {
		dx *= 3
		dy *= 3
	}
	for i := len(w.scrolls) - 1; i >= w.modalTop; i-- {
		s := w.scrolls[i]
		if contains(s.r, e.X, e.Y) {
			s.fn(dx, dy)
			w.tipID = ui.ID{}
			return ui.EventResult{Handled: true, Repaint: true}
		}
	}
	return ui.EventResult{Handled: true}
}

func (w *Window) onKey(e ui.KeyEvent) ui.EventResult {
	if e.Release {
		return ui.EventResult{}
	}
	handled := ui.EventResult{Handled: true, Repaint: true}
	if len(w.popups) > 0 {
		if w.popupKey(e) {
			return handled
		}
		return ui.EventResult{}
	}
	if w.dialog != nil {
		if fn, ok := w.keys[e.Target]; ok && fn(e) {
			return handled
		}
		if w.dialogKey(e) {
			return handled
		}
		return ui.EventResult{}
	}
	if fn, ok := w.keys[e.Target]; ok && fn(e) {
		return handled
	}
	if w.globalKey(e) {
		return handled
	}
	return ui.EventResult{}
}

func (w *Window) onShortcut(e ui.KeyEvent) ui.EventResult {
	if e.Release {
		return ui.EventResult{}
	}
	if w.shortcut(e) {
		return ui.EventResult{Handled: true, Repaint: true}
	}
	return ui.EventResult{}
}

func (w *Window) onFileDrop(e ui.FileDropEvent) ui.EventResult {
	if len(e.Paths) == 0 && e.Phase != ui.FileDropDrop {
		return ui.EventResult{Handled: true}
	}
	if e.Phase == ui.FileDropDrop && len(e.Paths) > 0 {
		w.openRepo(e.Paths[0])
		return ui.EventResult{Handled: true, Repaint: true}
	}
	return ui.EventResult{Handled: true}
}

func (w *Window) textInput(ctx app.TextInputContext) *app.TextInput {
	ed, ok := w.editors[ctx.ID]
	if !ok {
		return nil
	}
	if w.inputs == nil {
		w.inputs = make(map[ui.ID]*app.TextInput)
	}
	in := w.inputs[ctx.ID]
	if in == nil {
		in = &app.TextInput{}
		w.inputs[ctx.ID] = in
	}
	in.Editor = ed
	in.NavigationKeys = ctx.NavigationKeys
	return in
}

// ---- helpers -------------------------------------------------------------

// after defers a state change until the paint that detected it finishes.
// after schedules fn to run on the UI thread once this paint is over.
func (w *Window) after(fn func()) {
	if fn != nil {
		w.deferred = append(w.deferred, fn)
		if !w.painting {
			w.Invalidate() // called from an event handler: run on the next wake
		}
	}
}

// runDeferred runs the work scheduled by after, in order.
func (w *Window) runDeferred() {
	fns := w.deferred
	w.deferred = nil
	for _, fn := range fns {
		fn()
	}
}

// clicked reports a click on id and schedules fn.
func (w *Window) clicked(id ui.ID, fn func()) bool {
	if w.c.Clicked(id) {
		w.after(fn)
		return true
	}
	return false
}

func (w *Window) hovered(id ui.ID) bool { return w.c.Hovered() == id }
func (w *Window) pressed(id ui.ID) bool { return w.c.Pressed() == id }

func (w *Window) repo() *vapp.RepoState { return w.ws.Repo }

func (w *Window) openRepo(dir string) {
	w.ws.Open(dir, func(err error) {
		if err != nil {
			w.ws.NotifyError("Cannot open "+filepath.Base(dir), err)
		}
	})
}

func (w *Window) chooseRepo() {
	w.after(func() {
		dir, err := app.OpenDirectory(app.OpenDirectoryOptions{Title: "Open Git Repository"})
		if err != nil || dir == "" {
			return
		}
		w.openRepo(dir)
	})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func systemDark() bool { return app.SystemAppearance() != app.AppearanceLight }
