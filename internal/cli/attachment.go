package cli

import (
	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// attachmentCmd groups the attachment commands. Sending is --attach on ask
// and reply; this fetches what others attached.
func attachmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachment",
		Short: "Fetch files attached to requests and replies",
	}
	cmd.AddCommand(attachmentGetCmd())
	return cmd
}

func attachmentGetCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Download an attachment (to this agent's attachments directory, or -o path, or -o - for stdout)",
		Long: `Download an attachment by id.

Without -o the file is saved as <id>.<ext> in this agent's attachments
directory (beside its config, 0700, files 0600), and its path is printed.
The sender's file name is shown but never used to name the local file.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, cfg, err := connect()
			if err != nil {
				return err
			}
			data, d, err := r.FetchAttachment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			switch out {
			case "-":
				_, err := cmd.OutOrStdout().Write(data)
				return err
			case "":
				p, err := client.SaveAttachmentFile(client.AttachmentDir(cfg), args[0], d.MIME, data)
				if err != nil {
					return err
				}
				out = p
			default:
				if err := client.WritePrivateFile(out, data); err != nil {
					return err
				}
			}
			cmd.Printf("Saved %s (%s, %d bytes) to %s\n", args[0], client.MediaType(d.MIME), d.Size, out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write to this path instead (- for stdout)")
	return cmd
}
