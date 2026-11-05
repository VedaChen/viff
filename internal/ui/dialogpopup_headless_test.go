//go:build vivahead

package viffui

import (
	"testing"
	"time"
)

// A dropdown opened from a settings page must take clicks. The dialog traps the
// focus scope, and the input router only lets a region of that scope take a
// press, so a menu painted outside it swallowed every click and the form behind
// it kept the selection.
func TestDialogDropdownSelectsRow(t *testing.T) {
	f := newUIFixture(t)
	f.start(1440, 900)
	f.win.openSettings()
	f.win.Invalidate()
	f.s.Settle(60*time.Millisecond, 2*time.Second)

	f.click("Islands Dark") // the theme value opens its menu
	f.s.Settle(40*time.Millisecond, time.Second)

	row, ok := f.s.Hit("Islands Light")
	if !ok {
		t.Fatalf("the theme menu has no reachable row; texts: %v", f.s.Texts())
	}
	if err := f.s.Click(row.Bounds.X+row.Bounds.Width/2, row.Bounds.Y+row.Bounds.Height/2); err != nil {
		t.Fatalf("click the menu row: %v", err)
	}
	f.s.Settle(60*time.Millisecond, time.Second)

	f.click("OK")
	f.s.Settle(60*time.Millisecond, time.Second)
	if got := f.ws.Settings.Theme; got != "light" {
		t.Fatalf("Theme = %q, want light: the menu row did not take the click", got)
	}
}
