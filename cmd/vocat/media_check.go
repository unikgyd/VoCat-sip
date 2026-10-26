package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"vocat/internal/vowifi/ims"
)

func runMediaCheck(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("media-check", flag.ContinueOnError)
	flags.SetOutput(out)
	jsonOutput := flags.Bool("json", false, "write machine-readable local codec capabilities")
	requireAMR := flags.Bool("require-amr", false, "fail unless real AMR-NB encode/decode is ready")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: vocat media-check [--json] [--require-amr]")
	}
	status := ims.ProbeMediaCapabilities()
	if *jsonOutput {
		if err := json.NewEncoder(out).Encode(status); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(out, "Runtime: %s\nG.711: PCMA / PCMU\nAMR-NB: %s\nAMR-WB: not built\n", status.RuntimeFlavor, status.AMRNB.Code); err != nil {
			return err
		}
		if !status.AMRNB.Available {
			if status.AMRNB.Built {
				_, _ = fmt.Fprintln(out, "Install the matching libopencore-amrnb runtime (Debian/Ubuntu: libopencore-amrnb0), then retry.")
			} else {
				_, _ = fmt.Fprintln(out, "This static/platform build has no native AMR backend. Use the glibc-AMR build for IMS audio.")
			}
		}
	}
	if *requireAMR && !status.AMRNB.Available {
		return errors.New("AMR-NB encode/decode is unavailable")
	}
	return nil
}
