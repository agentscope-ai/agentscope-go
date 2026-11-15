package thread

import (
	"strings"
	"testing"
)

func TestDescriptorValidate(t *testing.T) {
	valid := Descriptor{Name: "edge", Zone: ZoneEdge, Window: 32000}
	cases := []struct {
		name   string
		mutate func(*Descriptor)
		want   string
	}{
		{"valid", func(*Descriptor) {}, ""},
		{"cloud with unknown window", func(d *Descriptor) { d.Zone, d.Window = ZoneCloud, 0 }, ""},
		{"missing name", func(d *Descriptor) { d.Name = " " }, "name is required"},
		{"slash in name", func(d *Descriptor) { d.Name = "edge/master" }, `may not contain "/"`},
		{"missing zone", func(d *Descriptor) { d.Zone = "" }, "zone"},
		{"unknown zone", func(d *Descriptor) { d.Zone = "moon" }, "zone"},
		{"negative window", func(d *Descriptor) { d.Window = -1 }, "negative window"},
		{"negative summary budget", func(d *Descriptor) { d.SummaryBudget = -1 }, "negative summary budget"},
		{"negative brief limit", func(d *Descriptor) { d.BriefMaxRunes = -1 }, "negative brief limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := valid
			tc.mutate(&d)
			err := d.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestRenderPeers(t *testing.T) {
	all := []Descriptor{
		{Name: "phone", Zone: ZoneEdge, Window: 8000, Description: "Reads the camera\nand the device files."},
		{Name: "cloud", Zone: ZoneCloud, Window: 200000, Description: "Writes code and searches the web.", ToolGroups: []string{"files"}},
		{Name: "edge", Zone: ZoneEdge, Window: 32000},
		{Name: "archive", Zone: ZoneCloud},
	}
	want := "Other participants in this session:\n" +
		"- archive (zone cloud, window unknown)\n" +
		"- cloud (zone cloud, window 200000 tokens): Writes code and searches the web.\n" +
		"- phone (zone edge, window 8000 tokens): Reads the camera and the device files."
	if got := RenderPeers("edge", all); got != want {
		t.Fatalf("RenderPeers\n got %q\nwant %q", got, want)
	}
	// Byte-stable whatever the input order.
	perms := [][]int{{3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}}
	for _, p := range perms {
		shuffled := make([]Descriptor, 0, len(all))
		for _, i := range p {
			shuffled = append(shuffled, all[i])
		}
		if got := RenderPeers("edge", shuffled); got != want {
			t.Errorf("order %v changed the text:\n%q", p, got)
		}
	}
	if got := RenderPeers("solo", []Descriptor{{Name: "solo", Zone: ZoneEdge}}); got != "" {
		t.Errorf("no peers gave %q", got)
	}
	if got := RenderPeers("x", nil); got != "" {
		t.Errorf("nil descriptors gave %q", got)
	}
	// Peers that share a name are still ordered deterministically.
	twins := []Descriptor{{Name: "t", Zone: ZoneEdge, Window: 2}, {Name: "t", Zone: ZoneEdge, Window: 1}}
	if RenderPeers("", twins) != RenderPeers("", []Descriptor{twins[1], twins[0]}) {
		t.Error("duplicate names render in input order")
	}
	// The input is not reordered.
	if all[0].Name != "phone" {
		t.Error("RenderPeers sorted its input")
	}
}
