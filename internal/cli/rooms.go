package cli

import (
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// roomCmd is tincan room: the owner creates rooms and picks their members
// from an admin device; members post to and read them as their agent.
func roomCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "room",
		Short: "Shared threads: the owner picks the members, and an @mention asks a member",
	}
	cmd.AddCommand(roomCreateCmd(), roomMemberCmd("add"), roomMemberCmd("remove"), roomPostCmd(), roomReadCmd(), roomListCmd())
	return cmd
}

// adminFlags adds --relay and --socket for a command only admin devices
// (or the relay host's admin socket) may run.
func adminFlags(cmd *cobra.Command, relayURL, socket *string) {
	cmd.Flags().StringVar(relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().StringVar(socket, "socket", "", "relay admin socket (when running on the relay host)")
}

func roomCreateCmd() *cobra.Command {
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "create <room>",
		Short: "Create an empty room (admin devices only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			if _, err := client.CreateRoom(cmd.Context(), r, args[0]); err != nil {
				return err
			}
			cmd.Printf("Created room %q. Add members with `tincan room add %s <agent>`.\n", args[0], args[0])
			return nil
		},
	}
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}

// roomMemberCmd is tincan room add or tincan room remove. Adding a member
// that sends what it reads to an outside vendor prints the relay's
// warning.
func roomMemberCmd(verb string) *cobra.Command {
	var relayURL, socket string
	short := "Add an agent to a room (admin devices only)"
	if verb == "remove" {
		short = "Take an agent out of a room (admin devices only)"
	}
	cmd := &cobra.Command{
		Use:   verb + " <room> <agent>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			room, agent := args[0], args[1]
			if verb == "remove" {
				res, err := client.RemoveRoomMember(cmd.Context(), r, room, agent)
				if err != nil {
					return err
				}
				if !res.Changed {
					cmd.Printf("%s was not in %s.\n", agent, room)
					return nil
				}
				cmd.Printf("Removed %s from %s.\n", agent, room)
				return nil
			}
			res, err := client.AddRoomMember(cmd.Context(), r, room, agent)
			if err != nil {
				return err
			}
			if res.Changed {
				cmd.Printf("Added %s to %s.\n", agent, room)
			} else {
				cmd.Printf("%s is already in %s.\n", agent, room)
			}
			if res.Warning != "" {
				cmd.Printf("Warning: %s. Remove it with `tincan room remove %s %s` if that is not what you want.\n", res.Warning, room, agent)
			}
			return nil
		},
	}
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}

func roomPostCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "post <room> <message...>",
		Short: "Post to a room as this agent; an @name of a member asks it, and its reply is posted to the room",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			res, err := client.PostToRoom(cmd.Context(), r, args[0], strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			cmd.Print(client.FormatRoomPostResponse(res))
			return nil
		},
	}
}

func roomReadCmd() *cobra.Command {
	var after int64
	var limit int
	cmd := &cobra.Command{
		Use:   "read <room>",
		Short: "Read a room's posts, oldest first, after a cursor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, _, err := connect()
			if err != nil {
				return err
			}
			defer printHeld(cmd, r)
			th, err := client.ReadRoom(cmd.Context(), r, args[0], after, limit)
			if err != nil {
				return err
			}
			cmd.Print(client.FormatRoomThread(th, time.Now()))
			return nil
		},
	}
	cmd.Flags().Int64Var(&after, "after", 0, "read only posts after this post id (the cursor the last read printed)")
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many posts (default and maximum 200)")
	return cmd
}

func roomListCmd() *cobra.Command {
	var all bool
	var relayURL, socket string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the rooms this agent is in, or with --all every room (admin devices only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var r *client.Relay
			var err error
			if all {
				r, err = adminRelay(socket, relayURL)
			} else {
				r, _, err = connect()
			}
			if err != nil {
				return err
			}
			rooms, err := client.Rooms(cmd.Context(), r, all)
			if err != nil {
				return err
			}
			if all && len(rooms) == 0 {
				cmd.Println("No rooms yet. Create one with `tincan room create <room>`.")
				return nil
			}
			cmd.Print(client.FormatRooms(rooms))
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every room (admin devices only)")
	adminFlags(cmd, &relayURL, &socket)
	return cmd
}
