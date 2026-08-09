// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// Giving a second person access to a self-hosted install (ADR-0084 §2).
//
// This is the step where "additional people, without cluster admin" stops being a design intention.
// Until now a colleague either got a copy of the install's shared token — which is everybody's
// token, cannot be revoked on its own, and needs a read of a Secret cluster RBAC guards — or got
// nothing.
//
// WHAT IS HANDED OVER IS NOT WHAT THEY END UP CARRYING. `auth invite` returns an invitation: it
// expires, it is spent the first time it is exchanged, and burrowd refuses it at every route but
// the exchange. The recipient runs `burrow auth login --invite`, and the credential they will
// actually operate with is generated on their machine, in that call, and never travels.
//
// That distinction is the whole reason this is two commands rather than one. Issuing a working
// token and sending it is one paste away from a credential living in a chat log, a mail server and
// a search index for as long as it is valid, and the person who pasted it has no way to tell.

func newAuthInviteCmd() *cobra.Command {
	o := &commonOpts{}
	var admin bool
	cmd := &cobra.Command{
		Use:   "invite <name>",
		Short: "Give somebody else access to this Burrow",
		Long: "invite records a person on this Burrow and prints an INVITATION for them.\n\n" +
			"They do not need cluster admin, and they never need the install's shared token. What they need\n" +
			"is a kubeconfig context that reaches the cluster, which is how the request travels, and this\n" +
			"invitation, which is how Burrow learns who they are.\n\n" +
			"The invitation is not a credential. It expires, it can be exchanged once, and Burrow refuses it\n" +
			"for anything else, so sending it over chat does not put a working token there. They exchange it\n" +
			"with `burrow auth login --context <cluster> --invite <invitation>`, and the credential they end\n" +
			"up carrying is created on their machine.\n\n" +
			"Inviting somebody already recorded issues them another invitation, which is what to do when the\n" +
			"first one expired. It does not change whether they are an admin.\n\n" +
			"Only an admin of this install can invite, and the first person to sign in is one.",
		Example: "  # Invite a colleague\n" +
			"  burrow auth invite ada\n\n" +
			"  # Invite somebody who will also give other people access\n" +
			"  burrow auth invite ada --admin",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			cred, err := c.InvitePrincipal(ctx, args[0], admin)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), inviteResult(cred), inviteLines(cmd.OutOrStdout(), cred))
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&admin, "admin", false,
		"let them invite other people and revoke credentials, as well as use this Burrow")
	return cmd
}

// authInviteResult is the JSON shape of `burrow auth invite`.
//
// IT CARRIES THE INVITATION IN THE CLEAR, and that is what the command is for: the string has to be
// readable to be handed over. It is the only value here worth protecting, and it protects itself by
// expiring and by being spent on first use — which is why this command can print one and the
// sign-in, which returns a real credential, prints none.
type authInviteResult struct {
	// Principal is the name recorded for them, which is what this install's audit trail will say.
	Principal string `json:"principal"`
	// PrincipalID is the opaque identity behind that name.
	PrincipalID string `json:"principalId,omitempty"`
	// Admin is whether they may invite other people in turn.
	Admin bool `json:"admin"`
	// ExpiresAt is when the invitation stops being exchangeable, RFC 3339.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// Invitation is the string to hand over. It is not a credential; see the type's own note.
	Invitation string `json:"invitation"`
}

func inviteResult(cred client.ClusterCredential) authInviteResult {
	return authInviteResult{
		Principal:   cred.Principal,
		PrincipalID: cred.PrincipalID,
		Admin:       cred.Admin,
		ExpiresAt:   cred.ExpiresAt,
		Invitation:  cred.Token,
	}
}

// inviteLines is the human output: who was recorded, the invitation, the exact command they run, and
// when it stops working.
//
// The command to send them is printed ready to copy, because the alternative is the sender writing
// it from memory and getting the flag wrong, and a recipient who cannot exchange an invitation asks
// for a working token instead — which is the thing this whole exchange exists to avoid.
//
// No em-dashes: this is user-facing CLI output.
func inviteLines(out io.Writer, cred client.ClusterCredential) string {
	name := cred.Principal
	if name == "" {
		name = "they"
	}
	// No full stop on the first line: the target clause is appended to it (actedon.go), so it reads
	// "invited ada on kube context …" rather than breaking mid-sentence.
	s := fmt.Sprintf("%s invited %s\n\n", okMark(out), name)
	s += "Send them this, and the command to run:\n\n"
	s += "  burrow auth login --context <their-kube-context> --invite " + cred.Token + "\n\n"
	s += "It is an invitation, not a credential: it can be exchanged once, only for a credential of\n" +
		"their own, and Burrow refuses it for anything else. The token they end up carrying is created\n" +
		"on their machine and is never sent anywhere.\n"
	if when := inviteExpiry(cred.ExpiresAt); when != "" {
		s += "\nIt stops working " + when + ". Run this again to issue another.\n"
	}
	if cred.Admin {
		s += "\nThey will be an admin of this Burrow, so they can invite other people too.\n"
	}
	return strings.TrimRight(s, "\n")
}

// inviteExpiry words when the invitation stops being exchangeable, in the reader's own time zone. An
// unparseable or absent value prints nothing rather than a guess: the sender is about to tell
// somebody how long they have, and a wrong answer there is worse than none.
func inviteExpiry(expiresAt string) string {
	if expiresAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return ""
	}
	return "on " + t.Local().Format("2 Jan 2006 at 15:04 MST")
}
