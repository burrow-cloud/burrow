// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package cloudcred

import (
	"fmt"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Transport builds the control-plane transport for a Burrow Cloud target: the stored credential of
// the given kind, presented as a bearer credential over HTTPS (ADR-0078 §1). There is no cluster in
// the path, so there is no API-server proxy to route through and no install Secret to read a token
// out of — the credential sign-in produced is the whole of the authentication.
//
// The kind is a parameter because the two binaries hold different credentials on purpose: `burrow`
// spends the person's and `burrow-agent` spends the agent's, so revoking one does not revoke the
// other (cloud ADR-0028 §2). Passing the wrong kind would spend the wrong credential, which is why
// Load also checks what the file says it is.
//
// The token is read here and goes straight into the transport. It is never returned, printed, or put
// in an error.
func Transport(baseURL string, kind Kind, clientName, version string) (client.Transport, error) {
	cred, err := Load(kind)
	if err != nil {
		return nil, err
	}
	return client.BearerTransport{
		BaseURL:  baseURL,
		Token:    cred.Token,
		Name:     clientName,
		Version:  version,
		Rejected: RejectedMessage(cred),
	}, nil
}

// RejectedMessage is what a refused credential says. A person who signed in yesterday and is
// unauthorized today has one likely explanation and one action, and both belong in the message: the
// credential was revoked, and signing in again replaces it. A bare 401 says neither.
//
// The credential's id is included so it can be matched against the console's list, which is the
// whole reason the id is stored. It is an identifier, not the token; the token appears nowhere.
func RejectedMessage(cred Credential) string {
	who := "this machine's credential"
	if cred.CredentialID != "" {
		who = fmt.Sprintf("this machine's credential (%s)", cred.CredentialID)
	}
	return fmt.Sprintf(
		"Burrow Cloud did not accept %s. It was most likely revoked — a credential can be revoked at any time from https://%s/settings — or the sign-in it came from no longer exists. Sign in again with \"burrow auth login\"",
		who, localconfig.CloudAPIEndpoint)
}
