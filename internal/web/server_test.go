package web

import "testing"

func TestFocusSwitchBanner(t *testing.T) {
	full := "0af0c1d2-3e4f-5678-9abc-def012345678"

	// Same focus → no banner; the persistent UI focus link already shows it.
	if got := focusSwitchBanner(full, full, "auth"); got != "" {
		t.Errorf("same focus should give no banner, got %q", got)
	}
	// Turn that touched no session → no banner.
	if got := focusSwitchBanner(full, "", "auth"); got != "" {
		t.Errorf("untouched turn should give no banner, got %q", got)
	}

	// Switch between sessions → "Switching to" + title + link.
	got := focusSwitchBanner("11111111-aaaa", full, "auth-service")
	want := "↪ Switching to [auth-service](#/s/" + full + ")\n\n"
	if got != want {
		t.Errorf("switch banner:\n got %q\nwant %q", got, want)
	}

	// First focus (none → X) → "Routing to".
	if got := focusSwitchBanner("", full, "auth-service"); got != "↪ Routing to [auth-service](#/s/"+full+")\n\n" {
		t.Errorf("first-focus banner = %q", got)
	}

	// Untitled session → short id as link text.
	if got := focusSwitchBanner("", full, ""); got != "↪ Routing to [0af0c1d2](#/s/"+full+")\n\n" {
		t.Errorf("untitled banner = %q", got)
	}
}
