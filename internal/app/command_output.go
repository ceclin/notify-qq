package app

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/ceclin/notify-qq/internal/apikey"
	"github.com/ceclin/notify-qq/internal/recipient"
)

func writeFields(output io.Writer, fields ...[2]string) error {
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	for _, field := range fields {
		if _, err := fmt.Fprintf(table, "%s:\t%s\n", field[0], field[1]); err != nil {
			return err
		}
	}
	return table.Flush()
}

func writeCreatedKey(output io.Writer, result apikey.CreateResult, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(output).Encode(result)
	}
	return writeFields(output,
		[2]string{"ID", result.ID},
		[2]string{"Name", result.Name},
		[2]string{"API key", result.APIKey},
		[2]string{"Pairing code", result.PairingCode},
		[2]string{"Expires at", *result.PairingExpiresAt},
	)
}

func writePairing(output io.Writer, result apikey.PairingResult, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(output).Encode(result)
	}
	return writeFields(output,
		[2]string{"ID", result.ID},
		[2]string{"Name", result.Name},
		[2]string{"Recipient", formatRecipient(result.Recipient)},
		[2]string{"Pairing code", result.PairingCode},
		[2]string{"Expires at", *result.PairingExpiresAt},
	)
}

func writeKeyList(output io.Writer, keys []apikey.Key, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(output).Encode(keys)
	}
	if len(keys) == 0 {
		_, err := fmt.Fprintln(output, "No API keys found.")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tNAME\tSTATUS\tRECIPIENT"); err != nil {
		return err
	}
	for _, key := range keys {
		status := "active"
		if key.RevokedAt != nil {
			status = "revoked"
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", key.ID, key.Name, status, formatRecipient(key.Recipient)); err != nil {
			return err
		}
	}
	return table.Flush()
}

func writeRevocation(output io.Writer, id string, changed, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(output).Encode(struct {
			ID      string `json:"id"`
			Revoked bool   `json:"revoked"`
			Changed bool   `json:"changed"`
		}{id, true, changed})
	}
	if !changed {
		_, err := fmt.Fprintf(output, "API key %s was already revoked.\n", id)
		return err
	}
	_, err := fmt.Fprintf(output, "Revoked API key %s.\n", id)
	return err
}

func formatRecipient(target *recipient.Target) string {
	if target == nil {
		return "unpaired"
	}
	return string(target.Type) + ":" + target.OpenID
}
