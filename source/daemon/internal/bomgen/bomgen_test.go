package bomgen

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/platform/release"
)

// The platforms a release publishes a daemon binary for.
var platforms = []string{"linux_amd64", "linux_arm64", "darwin_arm64"}

// The platforms the desktop app is bundled for: Apple silicon and Intel.
var desktopPlatforms = []string{"darwin_arm64", "darwin_amd64"}

func at(day int, hour int) time.Time {
	return time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC)
}

// digest stands in for the SHA-256 of an asset's bytes: the generator only
// copies it, so any stable hex string of the right shape exercises the path.
func digest(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func downloadURL(tag, name string) string {
	return "https://github.com/tumika/tumika/releases/download/" + tag + "/" + name
}

func rawAssets(tag, version string) []Asset {
	assets := make([]Asset, 0, len(platforms))
	for _, platform := range platforms {
		name := "tumika_" + version + "_" + platform
		assets = append(assets, Asset{
			Name:   name,
			URL:    downloadURL(tag, name),
			SHA256: digest(name),
		})
	}
	return assets
}

// minisign stands in for the text of a .sig file: the comment line and the
// base64 line Tauri's signer writes. The generator only copies it.
func minisign(name string) string {
	return "untrusted comment: signature from the tumika updater key\n" +
		base64.StdEncoding.EncodeToString([]byte(name)) + "\n"
}

// desktopAssets are the app's updater archive for each platform and the detached
// signature published beside it, exactly as the release workflow uploads them.
func desktopAssets(tag, version string) []Asset {
	assets := make([]Asset, 0, 2*len(desktopPlatforms))
	for _, platform := range desktopPlatforms {
		name := "tumika-desktop_" + version + "_" + platform + ".app.tar.gz"
		assets = append(assets,
			Asset{
				Name:      name,
				URL:       downloadURL(tag, name),
				SHA256:    digest(name),
				Signature: minisign(name),
			},
			Asset{
				Name:   name + ".sig",
				URL:    downloadURL(tag, name+".sig"),
				SHA256: digest(name + ".sig"),
			},
		)
	}
	return assets
}

func releaseSpec(label, daemonVersion string) []byte {
	return []byte("# a comment\nrelease: " + label + "\ncomponents:\n  daemon: " + daemonVersion + "\n")
}

// bothSpec is the release.yaml of a release shipping both components, whether or
// not it built either of them.
func bothSpec(label, daemonVersion, desktopVersion string) []byte {
	return []byte("release: " + label + "\ncomponents:\n  daemon: " + daemonVersion +
		"\n  desktop: " + desktopVersion + "\n")
}

// stable, beta and edge build the release each channel is cut from, complete
// and publishable; a test that is about one broken field breaks that field.
func stable(label string, published time.Time, daemonVersion string) Release {
	tag := "v" + label
	return Release{
		Tag:         tag,
		PublishedAt: published,
		Assets:      rawAssets(tag, daemonVersion),
		ReleaseYAML: releaseSpec(label, daemonVersion),
	}
}

func beta(label string, published time.Time, daemonVersion string) Release {
	rel := stable(label, published, daemonVersion)
	rel.Prerelease = true
	return rel
}

func edge(run, label string, published time.Time, daemonVersion string) Release {
	tag := "edge-" + run
	version := daemonVersion + "-edge." + run
	return Release{
		Tag:         tag,
		Prerelease:  true,
		PublishedAt: published,
		Assets:      rawAssets(tag, version),
		ReleaseYAML: releaseSpec(label, daemonVersion),
	}
}

// both is a stable release naming two components, built where a version is
// given and left to carry over where it is empty — which is how a release that
// changed only one component is spelled.
func both(label string, published time.Time, daemonVersion, desktopVersion string, built ...string) Release {
	rel := stable(label, published, daemonVersion)
	rel.ReleaseYAML = bothSpec(label, daemonVersion, desktopVersion)
	rel.Assets = nil
	for _, name := range built {
		switch name {
		case release.DaemonComponent:
			rel.Assets = append(rel.Assets, rawAssets(rel.Tag, daemonVersion)...)
		case release.DesktopComponent:
			rel.Assets = append(rel.Assets, desktopAssets(rel.Tag, desktopVersion)...)
		}
	}
	return rel
}

func generate(t *testing.T, releases ...Release) Result {
	t.Helper()
	result, err := Generate(releases)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return result
}

func channelDoc(t *testing.T, result Result, channel release.Channel) Document {
	t.Helper()
	for _, doc := range result.Channels {
		if doc.Channel == channel {
			return doc
		}
	}
	t.Fatalf("no head published for channel %s", channel)
	return Document{}
}

func parse(t *testing.T, doc Document) *release.BOM {
	t.Helper()
	bom, err := release.ParseBOM(doc.Bytes)
	if err != nil {
		t.Fatalf("%s does not parse: %v", doc.Path, err)
	}
	return bom
}

// A channel offers what every channel at least as vetted publishes, so one
// release reaches three heads without being republished.
func TestChannelsAreCumulative(t *testing.T) {
	result := generate(t,
		stable("2026.09.00", at(1, 9), "0.0.1"),
		beta("2026.09.01-beta.1", at(2, 9), "0.0.2-beta.1"),
		edge("7", "2026.09.01", at(3, 9), "0.0.2"),
	)

	want := map[release.Channel]string{
		release.ChannelStable: "2026.09.00",
		release.ChannelBeta:   "2026.09.01-beta.1",
		release.ChannelEdge:   "edge.7",
	}
	for channel, label := range want {
		doc := channelDoc(t, result, channel)
		if doc.Path != "channels/"+string(channel)+".json" {
			t.Errorf("%s head is served at %s", channel, doc.Path)
		}
		bom := parse(t, doc)
		if bom.Release != label {
			t.Errorf("%s head is release %s, want %s", channel, bom.Release, label)
		}
		// The daemon refuses a head that names a different channel than the one
		// it asked for, so a carried-up release is re-stated, not served as cut.
		if bom.Channel != channel {
			t.Errorf("%s head declares channel %s", channel, bom.Channel)
		}
	}

	if len(result.Releases) != 3 {
		t.Fatalf("published %d releases, want 3", len(result.Releases))
	}
	// A release's own document keeps the channel it was cut into.
	for _, doc := range result.Releases {
		bom := parse(t, doc)
		if doc.Path != "releases/"+bom.Release+".json" {
			t.Errorf("release %s is served at %s", bom.Release, doc.Path)
		}
	}
	if got := parse(t, result.Releases[0]).Channel; got != release.ChannelStable {
		t.Errorf("2026.09.00 is published as channel %s", got)
	}
}

// A more permissive channel is not offered to a stricter one: an edge build
// never reaches a stable daemon.
func TestAStricterChannelDoesNotReceiveALooserOne(t *testing.T) {
	result := generate(t,
		stable("2026.09.00", at(1, 9), "0.0.1"),
		edge("7", "2026.09.01", at(3, 9), "0.0.2"),
	)

	if got := parse(t, channelDoc(t, result, release.ChannelStable)).Release; got != "2026.09.00" {
		t.Errorf("stable head is %s, want 2026.09.00", got)
	}
	if got := parse(t, channelDoc(t, result, release.ChannelBeta)).Release; got != "2026.09.00" {
		t.Errorf("beta head is %s, want 2026.09.00", got)
	}
}

// A channel with nothing to offer has no document at all: the daemon reads a
// 404 as "nothing published", and an empty head is not a shape it accepts.
func TestAChannelWithNoReleaseIsOmitted(t *testing.T) {
	result := generate(t, beta("2026.09.01-beta.1", at(2, 9), "0.0.2-beta.1"))

	for _, doc := range result.Channels {
		if doc.Channel == release.ChannelStable {
			t.Fatalf("stable has a head at %s with no stable release published", doc.Path)
		}
	}
	if len(result.Channels) != 2 {
		t.Fatalf("published %d heads, want beta and edge", len(result.Channels))
	}
}

// Recency orders releases, and a label never does — so a head is chosen by
// publication time even when an older label was published last.
func TestHeadIsTheMostRecentlyPublished(t *testing.T) {
	result := generate(t,
		stable("2026.09.02", at(1, 9), "0.0.3"),
		stable("2026.09.01", at(5, 9), "0.0.2"),
	)

	if got := parse(t, channelDoc(t, result, release.ChannelStable)).Release; got != "2026.09.01" {
		t.Errorf("stable head is %s, want the later-published 2026.09.01", got)
	}
}

// A draft is not downloadable, so nothing may point at its assets, and it is
// ignored rather than reported as a skip.
func TestADraftIsNeverPublished(t *testing.T) {
	draft := stable("2026.09.02", at(9, 9), "0.0.3")
	draft.Draft = true

	result := generate(t, stable("2026.09.01", at(1, 9), "0.0.2"), draft)

	if len(result.Releases) != 1 {
		t.Fatalf("published %d releases, want only the non-draft", len(result.Releases))
	}
	if got := parse(t, channelDoc(t, result, release.ChannelStable)).Release; got != "2026.09.01" {
		t.Errorf("stable head is %s; the draft became the head", got)
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("skip report is %+v, want the draft absent: it is not a failure", result.Skipped)
	}
}

// A release missing anything a document needs is skipped whole and reported: a
// bill of materials naming an asset that is not there is a daemon that cannot
// update, so nothing is published half-built.
func TestAnIncompleteReleaseIsSkippedWithAReason(t *testing.T) {
	broken := func(mutate func(*Release)) Release {
		rel := stable("2026.09.02", at(9, 9), "0.0.3")
		mutate(&rel)
		return rel
	}

	tests := []struct {
		name   string
		rel    Release
		reason string
	}{
		{
			name:   "no release.yaml asset",
			rel:    broken(func(r *Release) { r.ReleaseYAML = nil }),
			reason: "no release.yaml asset",
		},
		{
			name:   "unreadable release.yaml",
			rel:    broken(func(r *Release) { r.ReleaseYAML = []byte("components:\n  daemon: 0.0.3\n") }),
			reason: "no top-level 'release:' key",
		},
		{
			name:   "release.yaml disagrees with the tag",
			rel:    broken(func(r *Release) { r.ReleaseYAML = releaseSpec("2026.09.01", "0.0.3") }),
			reason: "release.yaml names release 2026.09.01",
		},
		{
			name:   "no raw binary",
			rel:    broken(func(r *Release) { r.Assets = nil }),
			reason: "no tumika_0.0.3_<goos>_<goarch> asset",
		},
		{
			name: "only the compressed archive",
			rel: broken(func(r *Release) {
				r.Assets = []Asset{{
					Name:   "tumika_0.0.3_linux_amd64.tar.gz",
					URL:    "https://example.invalid/tumika_0.0.3_linux_amd64.tar.gz",
					SHA256: digest("archive"),
				}}
			}),
			reason: "no tumika_0.0.3_<goos>_<goarch> asset",
		},
		{
			name: "a component with no published asset name",
			rel: broken(func(r *Release) {
				r.ReleaseYAML = []byte("release: 2026.09.02\ncomponents:\n  daemon: 0.0.3\n  ledger: 1.0.0\n")
			}),
			reason: "component ledger publishes no asset name",
		},
		{
			name:   "a digest that is not a digest",
			rel:    broken(func(r *Release) { r.Assets[0].SHA256 = "nope" }),
			reason: "malformed",
		},
		{
			name:   "no publication time",
			rel:    broken(func(r *Release) { r.PublishedAt = time.Time{} }),
			reason: "no publication time",
		},
		{
			name:   "a tag that names no release",
			rel:    broken(func(r *Release) { r.Tag = "nightly" }),
			reason: "is not a release tag",
		},
		{
			name:   "a label that could escape a URL path",
			rel:    broken(func(r *Release) { r.Tag = "v../../etc" }),
			reason: "invalid release label",
		},
		{
			name:   "a stable tag marked prerelease",
			rel:    broken(func(r *Release) { r.Prerelease = true }),
			reason: "tag names a stable release",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := generate(t, stable("2026.09.01", at(1, 9), "0.0.2"), tc.rel)

			if len(result.Releases) != 1 {
				t.Fatalf("published %d releases, want only the complete one", len(result.Releases))
			}
			if len(result.Skipped) != 1 {
				t.Fatalf("skip report is %+v, want one entry", result.Skipped)
			}
			if result.Skipped[0].Tag != tc.rel.Tag {
				t.Errorf("skipped tag %s, want %s", result.Skipped[0].Tag, tc.rel.Tag)
			}
			if !strings.Contains(result.Skipped[0].Reason, tc.reason) {
				t.Errorf("skip reason %q does not mention %q", result.Skipped[0].Reason, tc.reason)
			}
		})
	}
}

// Two releases cannot claim one label: the second would overwrite the first
// document at the same path, and a signature would then cover bytes nobody
// reviewed.
func TestADuplicateLabelIsSkipped(t *testing.T) {
	first := stable("2026.09.01", at(1, 9), "0.0.2")
	second := stable("2026.09.01", at(2, 9), "0.0.3")

	result := generate(t, first, second)

	if len(result.Releases) != 1 {
		t.Fatalf("published %d documents for one label", len(result.Releases))
	}
	if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason, "already published") {
		t.Fatalf("skip report is %+v, want the duplicate reported", result.Skipped)
	}
}

// An unchanged component is not rebuilt: its entry points at the assets of the
// release that built it, so bytes are never republished under one component
// version.
func TestAnUnchangedComponentIsCarriedOver(t *testing.T) {
	built := stable("2026.09.00", at(1, 9), "0.0.1")
	carried := stable("2026.09.01", at(2, 9), "0.0.1")
	carried.Assets = nil
	again := stable("2026.09.02", at(3, 9), "0.0.1")
	again.Assets = nil

	result := generate(t, built, carried, again)

	if len(result.Skipped) != 0 {
		t.Fatalf("skip report is %+v, want nothing skipped", result.Skipped)
	}
	source := parse(t, result.Releases[0]).Components[release.DaemonComponent]
	if source.FromRelease != "" {
		t.Errorf("the release that built the daemon names from_release %s", source.FromRelease)
	}
	for _, doc := range result.Releases[1:] {
		bom := parse(t, doc)
		component := bom.Components[release.DaemonComponent]
		// The release that BUILT the component, not the last one to carry it
		// over: a chain of unchanged releases does not have to be walked to
		// reach the bytes.
		if component.FromRelease != "2026.09.00" {
			t.Errorf("%s carries the daemon from %q, want 2026.09.00", bom.Release, component.FromRelease)
		}
		for platform, asset := range component.Assets {
			if asset != source.Assets[platform] {
				t.Errorf("%s republishes %s at %s", bom.Release, platform, asset.URL)
			}
		}
	}
}

// A release that built both components names both, each at its own component
// version, and each from its own assets. The names are told apart by their
// extension alone, so the archives published beside them must not be mistaken
// for the assets the document points at.
func TestAReleaseThatBuildsBothComponentsPublishesBoth(t *testing.T) {
	rel := both("2026.09.01", at(2, 9), "0.0.2", "0.1.0", release.DaemonComponent, release.DesktopComponent)
	for _, platform := range platforms {
		name := "tumika_0.0.2_" + platform + ".tar.gz"
		rel.Assets = append(rel.Assets, Asset{Name: name, URL: downloadURL(rel.Tag, name), SHA256: digest(name)})
	}

	result := generate(t, rel)

	if len(result.Skipped) != 0 {
		t.Fatalf("skip report is %+v, want nothing skipped", result.Skipped)
	}
	bom := parse(t, result.Releases[0])
	daemon := bom.Components[release.DaemonComponent]
	if daemon.Version != "0.0.2" || len(daemon.Assets) != len(platforms) {
		t.Errorf("daemon is %s with %d assets, want 0.0.2 with %d", daemon.Version, len(daemon.Assets), len(platforms))
	}
	if daemon.FromRelease != "" {
		t.Errorf("the release that built the daemon names from_release %s", daemon.FromRelease)
	}

	desktop := bom.Components[release.DesktopComponent]
	if desktop.Version != "0.1.0" {
		t.Errorf("desktop component version is %s, want 0.1.0", desktop.Version)
	}
	if len(desktop.Assets) != len(desktopPlatforms) {
		t.Fatalf("desktop publishes %d assets, want %v", len(desktop.Assets), desktopPlatforms)
	}
	for _, platform := range desktopPlatforms {
		asset := desktop.Assets[platform]
		name := "tumika-desktop_0.1.0_" + platform + ".app.tar.gz"
		if !strings.HasSuffix(asset.URL, "/"+name) {
			t.Errorf("desktop %s is %s, want the archive %s", platform, asset.URL, name)
		}
		// The .sig is never an asset of its own: its text is the archive's
		// signature, which is the only thing the app's updater checks with.
		if asset.Signature != minisign(name) {
			t.Errorf("desktop %s carries signature %q", platform, asset.Signature)
		}
		if asset.SHA256 != digest(name) {
			t.Errorf("desktop %s hashes to %s, want the archive's digest", platform, asset.SHA256)
		}
	}
}

// A component the release did not build points at the assets of the release that
// did, whichever component that is: a hotfix builds only what changed.
func TestOnlyTheComponentAReleaseBuiltComesFromIt(t *testing.T) {
	tests := []struct {
		name    string
		built   []string
		carried string
	}{
		{
			name:    "a desktop-only release carries the daemon over",
			built:   []string{release.DesktopComponent},
			carried: release.DaemonComponent,
		},
		{
			name:    "a daemon-only release carries the desktop over",
			built:   []string{release.DaemonComponent},
			carried: release.DesktopComponent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := both("2026.09.00", at(1, 9), "0.0.1", "0.1.0",
				release.DaemonComponent, release.DesktopComponent)
			second := both("2026.09.01", at(2, 9), "0.0.1", "0.1.0", tc.built...)

			result := generate(t, first, second)

			if len(result.Skipped) != 0 {
				t.Fatalf("skip report is %+v, want nothing skipped", result.Skipped)
			}
			source, got := parse(t, result.Releases[0]), parse(t, result.Releases[1])
			carried := got.Components[tc.carried]
			if carried.FromRelease != "2026.09.00" {
				t.Errorf("%s comes from %q, want 2026.09.00", tc.carried, carried.FromRelease)
			}
			for platform, asset := range carried.Assets {
				if asset != source.Components[tc.carried].Assets[platform] {
					t.Errorf("%s republishes %s at %s", tc.carried, platform, asset.URL)
				}
			}
			for _, name := range tc.built {
				if from := got.Components[name].FromRelease; from != "" {
					t.Errorf("%s was built here and still names from_release %s", name, from)
				}
			}
		})
	}
}

// A release naming a component it neither built nor can carry over is skipped
// whole, and so is one whose signed asset arrived without its signature: a
// document pointing at an archive no updater can verify is worse than no
// document.
func TestADesktopComponentWithoutUsableAssetsIsSkipped(t *testing.T) {
	tests := []struct {
		name   string
		rel    Release
		reason string
	}{
		{
			name:   "no archive, and nothing earlier to carry over",
			rel:    both("2026.09.02", at(9, 9), "0.0.3", "0.1.1", release.DaemonComponent),
			reason: "no tumika-desktop_0.1.1_<goos>_<goarch>.app.tar.gz asset",
		},
		{
			name: "an archive published without its signature",
			rel: func() Release {
				rel := both("2026.09.02", at(9, 9), "0.0.3", "0.1.1",
					release.DaemonComponent, release.DesktopComponent)
				for i := range rel.Assets {
					rel.Assets[i].Signature = ""
				}
				return rel
			}(),
			reason: "tumika-desktop_0.1.1_darwin_arm64.app.tar.gz is published without its " +
				"tumika-desktop_0.1.1_darwin_arm64.app.tar.gz.sig",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := generate(t, both("2026.09.01", at(1, 9), "0.0.2", "0.1.0",
				release.DaemonComponent, release.DesktopComponent), tc.rel)

			if len(result.Releases) != 1 {
				t.Fatalf("published %d releases, want only the complete one", len(result.Releases))
			}
			if len(result.Skipped) != 1 {
				t.Fatalf("skip report is %+v, want one entry", result.Skipped)
			}
			if !strings.Contains(result.Skipped[0].Reason, tc.reason) {
				t.Errorf("skip reason %q does not mention %q", result.Skipped[0].Reason, tc.reason)
			}
		})
	}
}

// A release shipping only the daemon publishes the document it always did: no
// component gains a field because another component has one, and the daemon's
// own reader accepts it unchanged.
func TestADaemonOnlyDocumentCarriesNoSignature(t *testing.T) {
	result := generate(t, stable("2026.09.01", at(2, 9), "0.0.2"))

	doc := result.Releases[0]
	bom, err := release.ParseBOM(doc.Bytes)
	if err != nil {
		t.Fatalf("%s does not parse: %v", doc.Path, err)
	}
	if len(bom.Components) != 1 {
		t.Fatalf("document names %d components, want the daemon alone", len(bom.Components))
	}
	if strings.Contains(string(doc.Bytes), "signature") {
		t.Errorf("a daemon-only document carries a signature field:\n%s", doc.Bytes)
	}
	for _, asset := range bom.Components[release.DaemonComponent].Assets {
		if asset.Signature != "" {
			t.Errorf("daemon asset %s carries a signature", asset.URL)
		}
	}
}

// A draft's assets are not downloadable, so a later release must not carry a
// component over from one: the entry would point at a URL that 404s and the
// draft's own document is never published to explain it.
func TestADraftIsNeverACarryOverSource(t *testing.T) {
	draft := both("2026.09.01", at(1, 9), "0.0.2", "0.1.0",
		release.DaemonComponent, release.DesktopComponent)
	draft.Draft = true
	carrier := both("2026.09.02", at(2, 9), "0.0.2", "0.1.0", release.DaemonComponent)

	result := generate(t, draft, carrier)

	if len(result.Releases) != 0 {
		t.Fatalf("published %d releases, want none: the carrier can only point at the draft", len(result.Releases))
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Tag != carrier.Tag {
		t.Fatalf("skip report is %+v, want %s alone", result.Skipped, carrier.Tag)
	}
	if !strings.Contains(result.Skipped[0].Reason, "no earlier release publishes desktop 0.1.0") {
		t.Errorf("skip reason %q does not name the missing desktop", result.Skipped[0].Reason)
	}
}

// An edge build's component version carries the run number of the build that
// produced it, so two edge builds of one commit never claim the same component
// version — and the assets are named by the version they carry.
func TestAnEdgeBuildIsVersionedByItsRun(t *testing.T) {
	result := generate(t, edge("41", "2026.09.01", at(3, 9), "0.0.2"))

	bom := parse(t, channelDoc(t, result, release.ChannelEdge))
	component := bom.Components[release.DaemonComponent]
	if component.Version != "0.0.2-edge.41" {
		t.Fatalf("edge daemon version is %s, want 0.0.2-edge.41", component.Version)
	}
	asset, ok := bom.Asset(release.DaemonComponent, "linux", "arm64")
	if !ok {
		t.Fatal("the edge head publishes no linux_arm64 daemon")
	}
	if !strings.HasSuffix(asset.URL, "tumika_0.0.2-edge.41_linux_arm64") {
		t.Errorf("edge asset is %s, want the run-stamped name", asset.URL)
	}
}

// An edge workflow that already stamped the run number into release.yaml must
// not have it stamped twice.
func TestAnEdgeVersionIsStampedOnce(t *testing.T) {
	rel := edge("41", "2026.09.01", at(3, 9), "0.0.2")
	rel.ReleaseYAML = releaseSpec("2026.09.01", "0.0.2-edge.41")

	result := generate(t, rel)

	if got := parse(t, result.Releases[0]).Components[release.DaemonComponent].Version; got != "0.0.2-edge.41" {
		t.Errorf("edge daemon version is %s, want 0.0.2-edge.41", got)
	}
}

// A publish is retried by running the generator again, so the same releases
// must produce byte-identical documents however they were ordered and whatever
// time zone the API reported.
func TestGenerationIsDeterministic(t *testing.T) {
	releases := []Release{
		stable("2026.09.00", at(1, 9), "0.0.1"),
		beta("2026.09.01-beta.1", at(2, 9), "0.0.2-beta.1"),
		edge("7", "2026.09.01", at(3, 9), "0.0.2"),
	}
	elsewhere := make([]Release, len(releases))
	copy(elsewhere, releases)
	elsewhere[0], elsewhere[2] = elsewhere[2], elsewhere[0]
	elsewhere[1].PublishedAt = elsewhere[1].PublishedAt.In(time.FixedZone("UTC+7", 7*60*60))

	first := generate(t, releases...)
	second := generate(t, elsewhere...)

	bytesByPath := func(result Result) map[string]string {
		out := map[string]string{}
		for _, doc := range append(append([]Document{}, result.Releases...), result.Channels...) {
			out[doc.Path] = string(doc.Bytes)
		}
		return out
	}
	one, other := bytesByPath(first), bytesByPath(second)
	if len(one) != len(other) {
		t.Fatalf("%d documents, then %d", len(one), len(other))
	}
	for path, body := range one {
		if other[path] != body {
			t.Errorf("%s differs between runs:\n%s\n%s", path, body, other[path])
		}
	}
}

// The exact bytes that are published and signed. Key order, indentation and the
// trailing newline are part of the contract: a signature covers these bytes,
// and a re-run that re-encoded them differently would invalidate it.
const wantDocument = `{
  "release": "2026.09.01",
  "channel": "stable",
  "published_at": "2026-09-02T09:00:00Z",
  "components": {
    "daemon": {
      "version": "0.0.2",
      "assets": {
        "darwin_arm64": {
          "url": "https://github.com/tumika/tumika/releases/download/v2026.09.01/tumika_0.0.2_darwin_arm64",
          "sha256": "%s"
        }
      }
    }
  }
}
`

func TestPublishedBytes(t *testing.T) {
	rel := stable("2026.09.01", at(2, 9), "0.0.2")
	rel.Assets = rel.Assets[len(rel.Assets)-1:] // darwin_arm64 alone, to keep the document readable

	result := generate(t, rel)

	got := string(result.Releases[0].Bytes)
	want := fmt.Sprintf(wantDocument, digest("tumika_0.0.2_darwin_arm64"))
	if got != want {
		t.Errorf("published bytes:\n%s\nwant:\n%s", got, want)
	}
	if !strings.HasSuffix(got, "}\n") {
		t.Error("a published document ends in a newline")
	}
}

// Every document the generator emits is read back by the daemon's own parser,
// so a producer and a reader cannot drift.
func TestEveryDocumentIsReadableByTheDaemon(t *testing.T) {
	result := generate(t,
		stable("2026.09.00", at(1, 9), "0.0.1"),
		beta("2026.09.01-beta.1", at(2, 9), "0.0.2-beta.1"),
		edge("7", "2026.09.01", at(3, 9), "0.0.2"),
	)

	for _, doc := range append(append([]Document{}, result.Releases...), result.Channels...) {
		bom := parse(t, doc)
		if bom.Release != doc.Release || bom.Channel != doc.Channel {
			t.Errorf("%s: document says %s/%s, body says %s/%s",
				doc.Path, doc.Release, doc.Channel, bom.Release, bom.Channel)
		}
		for _, platform := range platforms {
			goos, goarch, _ := strings.Cut(platform, "_")
			if _, ok := bom.Asset(release.DaemonComponent, goos, goarch); !ok {
				t.Errorf("%s publishes no daemon for %s", doc.Path, platform)
			}
		}
	}
}

func TestParseTag(t *testing.T) {
	tests := []struct {
		tag     string
		label   string
		channel release.Channel
		wantErr bool
	}{
		{tag: "v2026.09.01", label: "2026.09.01", channel: release.ChannelStable},
		{tag: "v2026.09.01-beta.2", label: "2026.09.01-beta.2", channel: release.ChannelBeta},
		{tag: "edge-147", label: "edge.147", channel: release.ChannelEdge},
		{tag: "vedge.147", wantErr: true},
		{tag: "2026.09.01", wantErr: true},
		{tag: "v2026.9.1", wantErr: true},
		{tag: "edge-x", wantErr: true},
		{tag: "v../../etc/passwd", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			label, channel, err := ParseTag(tc.tag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTag(%q) = %q/%s, want an error", tc.tag, label, channel)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTag(%q): %v", tc.tag, err)
			}
			if label != tc.label || channel != tc.channel {
				t.Errorf("ParseTag(%q) = %q/%s, want %q/%s", tc.tag, label, channel, tc.label, tc.channel)
			}
		})
	}
}

// The generator reads a release's label and component versions out of the
// release.yaml the release was built from, the same file and the same parse the
// release scripts do — so the repo's own file must be one it can read.
func TestParseTheCommittedReleaseYAML(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "release.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // a fixed path under the repo
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	spec, err := parseReleaseYAML(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if err := release.ValidateReleaseLabel(spec.label); err != nil {
		t.Errorf("%s names %q: %v", path, spec.label, err)
	}
	if _, ok := spec.components[release.DaemonComponent]; !ok {
		t.Errorf("%s names no %s component, only %v", path, release.DaemonComponent, spec.componentNames())
	}
	// A component the generator cannot name an asset for skips every release that
	// ships it, so adding one to release.yaml adds a row to componentAssets.
	for _, name := range spec.componentNames() {
		if _, ok := componentAssets[name]; !ok {
			t.Errorf("%s names component %s, whose assets this generator cannot name", path, name)
		}
	}
}

func TestParseReleaseYAML(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		label   string
		daemon  string
		wantErr bool
	}{
		{
			name:   "comments and quoting",
			body:   "# header\nrelease: \"2026.09.01\" # the label\ncomponents:\n  daemon: '0.0.2' # the daemon\n",
			label:  "2026.09.01",
			daemon: "0.0.2",
		},
		{
			name:   "a key below the components block is not a component",
			body:   "release: 2026.09.01\ncomponents:\n  daemon: 0.0.2\nnotes:\n  daemon: nonsense\n",
			label:  "2026.09.01",
			daemon: "0.0.2",
		},
		{
			name:    "an indented release key is not the label",
			body:    "notes:\n  release: 2026.09.01\ncomponents:\n  daemon: 0.0.2\n",
			wantErr: true,
		},
		{
			name:    "a label the daemon would refuse",
			body:    "release: 2026.9.1\ncomponents:\n  daemon: 0.0.2\n",
			wantErr: true,
		},
		{
			name:    "no components",
			body:    "release: 2026.09.01\ncomponents:\n",
			wantErr: true,
		},
		{
			name:    "two entries for one component",
			body:    "release: 2026.09.01\ncomponents:\n  daemon: 0.0.2\n  daemon: 0.0.3\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseReleaseYAML([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsed %+v, want an error", spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReleaseYAML: %v", err)
			}
			if spec.label != tc.label {
				t.Errorf("label %q, want %q", spec.label, tc.label)
			}
			if spec.components[release.DaemonComponent] != tc.daemon {
				t.Errorf("daemon version %q, want %q", spec.components[release.DaemonComponent], tc.daemon)
			}
		})
	}
}
