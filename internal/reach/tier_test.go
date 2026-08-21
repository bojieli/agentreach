package reach

import (
	"strings"
	"testing"
)

// The tier vocabulary appears in a flag's help, in error messages, in session
// files on disk and in an operator's muscle memory. Renaming `agent` to
// `helper` updated some of those and not others, so --fileops went on
// advertising a value ParseTier had already started rejecting. These tests hold
// the spellings together.

func TestEveryTierHasAName(t *testing.T) {
	for _, tier := range AllTiers {
		name, ok := tierNames[tier]
		if !ok {
			t.Errorf("tier %d is in AllTiers but has no name", int(tier))
			continue
		}
		if name == "" || strings.Contains(name, "tier(") {
			t.Errorf("tier %d renders as %q", int(tier), name)
		}
	}
	if len(tierNames) != len(AllTiers) {
		t.Errorf("%d names for %d tiers; one was added to a list and not the other",
			len(tierNames), len(AllTiers))
	}
}

// Anything printed as a tier must be accepted back. This is the property that
// broke: reach told operators about a tier it would then refuse.
func TestEveryPrintedTierParsesBack(t *testing.T) {
	for _, tier := range AllTiers {
		got, err := ParseTier(tier.String())
		if err != nil {
			t.Errorf("ParseTier(%q) rejects a tier reach prints: %v", tier.String(), err)
			continue
		}
		if got != tier {
			t.Errorf("ParseTier(%q) = %v, want %v", tier.String(), got, tier)
		}
	}
}

// TierList is what the --fileops help text is built from, so every name in it
// has to be one an operator can actually pass.
func TestTierListNamesOnlyUsableTiers(t *testing.T) {
	list := TierList()
	for _, name := range strings.Split(list, ", ") {
		if _, err := ParseTier(name); err != nil {
			t.Errorf("TierList() offers %q, which ParseTier rejects: %v", name, err)
		}
	}
	for _, tier := range AllTiers {
		if !strings.Contains(list, tier.String()) {
			t.Errorf("TierList() = %q, missing %q", list, tier)
		}
	}
}

// The order carries meaning: autonegotiation steps down from a tier to the one
// below, so a list out of capability order would degrade upward.
func TestAllTiersIsInAscendingOrder(t *testing.T) {
	for i := 1; i < len(AllTiers); i++ {
		if AllTiers[i] <= AllTiers[i-1] {
			t.Fatalf("AllTiers is not ascending at %d: %v then %v",
				i, AllTiers[i-1], AllTiers[i])
		}
	}
	if AllTiers[0] != TierPOSIX {
		t.Errorf("AllTiers starts at %v; posix is the floor and must be first", AllTiers[0])
	}
}

// A name reach used to accept must explain itself rather than being reported as
// unknown. An operator who pinned a tier deserves to know what happened to it,
// not to wonder whether they mistyped.
func TestRetiredTierNamesExplainThemselves(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"sftp", "removed"},
		{"agent", "helper"},
	} {
		_, err := ParseTier(tc.name)
		if err == nil {
			t.Errorf("ParseTier(%q) succeeded; that tier no longer exists", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseTier(%q) does not explain itself: %v", tc.name, err)
		}
		if strings.Contains(err.Error(), "unknown fileops tier") {
			t.Errorf("ParseTier(%q) reports a name it used to accept as simply unknown", tc.name)
		}
	}
}

func TestParseTierRejectsNonsense(t *testing.T) {
	for _, name := range []string{"", "POSIX", "Helper", "pipe ", "nfs"} {
		if _, err := ParseTier(name); err == nil {
			t.Errorf("ParseTier(%q) succeeded", name)
		}
	}
}

// An unnamed tier value must still render as something diagnosable rather than
// as another tier's name.
func TestUnknownTierRendersDistinctly(t *testing.T) {
	got := Tier(99).String()
	if !strings.Contains(got, "99") {
		t.Errorf("Tier(99).String() = %q, want it to name the value", got)
	}
	for _, tier := range AllTiers {
		if got == tier.String() {
			t.Fatalf("Tier(99) renders as %q, the same as a real tier", got)
		}
	}
}
