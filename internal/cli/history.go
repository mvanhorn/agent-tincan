package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// historyNow is the clock for history reads; tests pin it.
var historyNow = time.Now

// historyReader returns the local reader for a source name.
func historyReader(source string) (history.Reader, error) {
	switch history.Source(source) {
	case history.SourceCodex:
		r := history.NewCodex()
		r.Now = historyNow
		return r, nil
	case history.SourceClaudeCode:
		r := history.NewClaudeCode()
		r.Now = historyNow
		return r, nil
	}
	return nil, fmt.Errorf("unknown history source %q (want codex or claude-code)", source)
}

func historyCmd() *cobra.Command {
	var list int
	var all, latest, asJSON bool
	var search, id, imagesDir string
	cmd := &cobra.Command{
		Use:   "history <codex|claude-code>",
		Short: "Read Matt's local Codex or Claude Code history",
		Long: "Read Matt's local Codex or Claude Code history. With no mode flag it shows the latest prompt Matt typed.\n" +
			"Unattended runs (codex exec wakes, Claude Code SDK sessions) are left out unless --all is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			modes := 0
			if cmd.Flags().Changed("list") {
				modes++
			}
			for _, on := range []bool{latest, search != "", id != ""} {
				if on {
					modes++
				}
			}
			if modes > 1 {
				return errors.New("use only one of --list, --latest, --search, --id")
			}
			r, err := historyReader(args[0])
			if err != nil {
				return err
			}
			opts := history.Options{All: all}
			var convs []history.Conversation
			if cmd.Flags().Changed("list") {
				if list <= 0 || list > 200 {
					return errors.New("--list must be between 1 and 200")
				}
				convs, err = r.List(cmd.Context(), list, opts)
			} else {
				q := history.Query{Source: r.Source(), Mode: history.ModeLatest, WantImages: imagesDir != ""}
				switch {
				case search != "":
					q.Mode, q.Terms = history.ModeSearch, strings.Fields(search)
				case id != "":
					q.Mode, q.ConversationID = history.ModeConversation, id
				}
				if err := q.Validate(); err != nil {
					return err
				}
				convs, err = r.Read(cmd.Context(), q, opts)
				if err == nil && imagesDir != "" {
					err = history.SaveImages(imagesDir, convs)
				}
			}
			if err != nil {
				return err
			}
			if asJSON {
				if convs == nil {
					convs = []history.Conversation{}
				}
				b, err := json.MarshalIndent(convs, "", "  ")
				if err != nil {
					return err
				}
				cmd.Println(string(b))
				return nil
			}
			printHistory(cmd, convs, cmd.Flags().Changed("list"))
			return nil
		},
	}
	cmd.Flags().IntVar(&list, "list", 20, "list the N most recent conversations with their working directory")
	cmd.Flags().BoolVar(&all, "all", false, "include unattended runs (codex exec, Claude Code SDK)")
	cmd.Flags().BoolVar(&latest, "latest", false, "show the latest prompt and its reply (default)")
	cmd.Flags().StringVar(&search, "search", "", "find recent conversations whose title or a prompt contains every word")
	cmd.Flags().StringVar(&id, "id", "", "show one conversation by id")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().StringVar(&imagesDir, "images-dir", "", "save the selected turn's images here (created 0700, files 0600)")
	return cmd
}

func printHistory(cmd *cobra.Command, convs []history.Conversation, listing bool) {
	if len(convs) == 0 {
		cmd.Println("Nothing found.")
		return
	}
	for i, c := range convs {
		if listing {
			cwd := c.Cwd
			if cwd == "" {
				cwd = "-"
			}
			flag := ""
			if c.Automated {
				flag = "  [" + c.Originator + "]"
			}
			cmd.Printf("%s  %s  %s  %s%s\n", c.UpdatedAt.Local().Format("2006-01-02 15:04"), c.ID, cwd, oneLine(c.Title), flag)
			continue
		}
		if i > 0 {
			cmd.Println()
		}
		cmd.Printf("%s  %s  %s\n", c.Source, c.ID, c.Title)
		if c.Cwd != "" {
			cmd.Printf("cwd: %s\n", c.Cwd)
		}
		if c.Automated {
			cmd.Printf("originator: %s (unattended)\n", c.Originator)
		}
		for _, m := range c.Messages {
			ts := ""
			if !m.Time.IsZero() {
				ts = " " + m.Time.Local().Format("2006-01-02 15:04")
			}
			cmd.Printf("[%s%s] %s\n", m.Role, ts, m.Text)
			for _, im := range m.Images {
				cmd.Printf("  image: %s (%s, %d bytes)\n", im.Path, im.MIME, im.Size)
			}
		}
	}
}
