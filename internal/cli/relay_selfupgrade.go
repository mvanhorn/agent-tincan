package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// latestReleaseURL is GitHub's newest stable release of this project.
// releases/latest skips prereleases and drafts. Tests replace it.
var latestReleaseURL = "https://api.github.com/repos/mvanhorn/agent-tincan/releases/latest"

// applySelfUpgrade is tincan relay install --self-upgrade: it sets --dist to
// <state-dir>/dist and --release-url to this project's releases, keeping
// either one the operator gave, and creates the dist folder. The service
// then carries the same explicit flags an operator could write by hand.
func applySelfUpgrade(fs *pflag.FlagSet, stateDir string) error {
	if !fs.Changed("dist") {
		if err := fs.Set("dist", filepath.Join(stateDir, "dist")); err != nil {
			return err
		}
	}
	if !fs.Changed("release-url") {
		if err := fs.Set("release-url", GitHubReleaseURL); err != nil {
			return err
		}
	}
	return os.MkdirAll(fs.Lookup("dist").Value.String(), 0o755)
}

// selfUpgradeOffer is what relay install says when it writes a relay that
// cannot upgrade itself.
func selfUpgradeOffer(dist string) string {
	if dist != "" {
		return ""
	}
	return "Self-upgrade is off: tincan relay-upgrade cannot update this relay, and agents cannot get new releases from it with tincan upgrade. " +
		"To turn it on, run this install again with --self-upgrade (it adds --dist <state-dir>/dist and --release-url " + GitHubReleaseURL + "). " +
		"The relay still downloads only a release an admin names with tincan relay-upgrade.\n"
}

// selfUpgradeWarning says why tincan relay-upgrade would fail on this relay:
// its user cannot replace its binary, or cannot write the dist folder that
// --from-github downloads into. Empty when it can.
func selfUpgradeWarning(exe, dist string) string {
	var problems []string
	if err := writable(exe); err != nil {
		problems = append(problems, err.Error())
	}
	if dist != "" {
		if err := dirWritable(dist); err != nil {
			problems = append(problems, fmt.Sprintf("the dist folder %s is not writable by the relay user (%v)", dist, err))
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return "warning: tincan relay-upgrade will fail on this relay until this is fixed: " + strings.Join(problems, "; ")
}

// latestReleaseTag asks GitHub for the newest stable release's tag, on the
// machine running tincan relay-upgrade, so the relay itself never picks a
// release. The tag must look like a release tag before it is used.
func latestReleaseTag(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up the latest release: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("look up the latest release: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("look up the latest release: GitHub answered %s", resp.Status)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("look up the latest release: %w", err)
	}
	if !releaseTag.MatchString(rel.Tag) {
		return "", fmt.Errorf("the latest release has tag %q, which is not a release tag such as v0.8.0; pass --from-github with the tag you want", rel.Tag)
	}
	return rel.Tag, nil
}
