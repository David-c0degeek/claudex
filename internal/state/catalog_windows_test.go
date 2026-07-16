//go:build windows

package state

import "testing"

// On Windows, reserved device names and volume-qualified forms are not local and
// must be rejected as run-directory locators.
func TestCatalogRejectsWindowsReservedLocators(t *testing.T) {
	c := newCatalog(t)
	bad := []string{"NUL", "CON", "runs/NUL", "C:/outside", "C:outside"}
	for _, d := range bad {
		if _, err := c.Allocate(0, ref("r", d)); err == nil {
			t.Fatalf("windows locator %q should be rejected", d)
		}
	}
}
