package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/Asere231/nimbus/internal/authz"
)

// `nimbus can` -- asking the question instead of discovering the answer
// through a failed apply.
//
// It asks the RUNNING server, over the same /api/authz endpoint the
// inspector uses, rather than evaluating anything itself. That is the
// whole design: the thing that answers is the thing that decides, so a
// yes here cannot disagree with what a request gets. A second evaluator
// living in the CLI would be a second set of bugs, and the one place
// nobody would think to look when they disagreed.
//
// Usage:
//
//	nimbus can ACTION RESOURCE [--principal ID]
//	nimbus can --principal serviceAccount:app@… storage.objects.create nimbus-data
//
// With no --principal it answers for the default identity, which on every
// cloud is unrestricted -- so the interesting form always names one.

func canCLI(args []string) {
	fs := flag.NewFlagSet("can", flag.ExitOnError)
	uiURL := fs.String("ui", "http://localhost:4700", "the running visualizer to ask")
	principal := fs.String("principal", "",
		"who to ask about: an IAM role or user name, an Azure object id, or a GCP member")
	quiet := fs.Bool("quiet", false, "print nothing; the exit status is the answer")
	_ = fs.Parse(args)

	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: nimbus can [--principal ID] ACTION RESOURCE")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  nimbus can --principal reader s3:PutObject arn:aws:s3:::demo/a.txt")
		fmt.Fprintln(os.Stderr, "  nimbus can --principal 22222222-2222-2222-2222-222222222222 \\")
		fmt.Fprintln(os.Stderr, "      Microsoft.Network/virtualNetworks/write \\")
		fmt.Fprintln(os.Stderr, "      /subscriptions/.../virtualNetworks/vnet")
		os.Exit(2)
	}
	action, resource := fs.Arg(0), fs.Arg(1)

	q := url.Values{"resource": {resource}, "action": {action}}
	if *principal != "" {
		q.Set("principal", *principal)
	}
	res, err := http.Get(*uiURL + "/api/authz?" + q.Encode())
	if err != nil {
		log.Fatalf("asking %s: %v\n\nIs Nimbus running?", *uiURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		log.Fatalf("no resource with id %q. Nimbus answers about resources it holds, "+
			"so create it first (or check the id in the visualizer).", resource)
	}
	if res.StatusCode != http.StatusOK {
		log.Fatalf("asking %s: unexpected status %d", *uiURL, res.StatusCode)
	}

	var report authz.Report
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		log.Fatalf("reading the answer: %v", err)
	}
	if len(report.Checks) == 0 {
		log.Fatalf("Nimbus cannot answer for %q: its cloud has no evaluator, "+
			"or that action is not one it can name.", resource)
	}
	check := report.Checks[0]

	// The exit status IS the answer, so this is usable in a script and in
	// CI: 0 allowed, 1 denied, 2 cannot tell. "Cannot tell" gets its own
	// status rather than being folded into denied, for the same reason the
	// engine keeps them apart.
	status := 1
	switch {
	case check.Allowed:
		status = 0
	case check.Unevaluated:
		status = 2
	}

	if !*quiet {
		who := report.Principal
		if who == "" {
			who = "the default identity"
		}
		verdict := "DENIED"
		switch {
		case check.Allowed:
			verdict = "ALLOWED"
		case check.Unevaluated:
			verdict = "UNKNOWN"
		}
		fmt.Printf("%s  %s\n", verdict, check.Action)
		fmt.Printf("  principal  %s\n", who)
		fmt.Printf("  resource   %s\n", resource)
		if check.Rule != "" {
			fmt.Printf("  rule       %s\n", check.Rule)
		}
		fmt.Printf("  because    %s\n", check.Reason)
		if report.Unrestricted {
			fmt.Printf("\n  Note: this principal is unrestricted, so no policy was evaluated.\n")
		}
		if !report.Enforcing {
			fmt.Printf("\n  Note: --enforce-iam is OFF, so this is what WOULD happen.\n" +
				"        Nothing is actually being refused.\n")
		}
	}
	os.Exit(status)
}
