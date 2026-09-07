// Command sign prints the signature for a request, using the same
// ISTORE_KEY / ISTORE_SALT the server reads.
//
//	ISTORE_KEY=$KEY ISTORE_SALT=$SALT sign '/photo.jpg?x-oss-process=image/resize,w_800'
//
// The argument is the URL as the client will send it: the object path, and the
// x-oss-process value if there is one. What comes back goes in the
// x-istore-signature query parameter.
//
//	/photo.jpg?x-oss-process=image/resize,w_800&x-istore-signature=<output>
//
// Order does not matter — the server reads both parameters by name — but the
// process chain has to match byte for byte, including the case and the order of
// its own parameters, because that string is what was signed.
package main

import (
	"fmt"
	"net/url"
	"os"

	"github.com/kane/istore/internal/httpserver"
	"github.com/kane/istore/internal/security"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s '<path>[?x-oss-process=<chain>]'\n", os.Args[0])
		os.Exit(2)
	}

	sc := security.NewDefaultConfig()
	if _, err := security.LoadConfigFromEnv(&sc); err != nil {
		die(err)
	}
	checker, err := security.New(&sc)
	if err != nil {
		die(err)
	}

	u, err := url.Parse(os.Args[1])
	if err != nil {
		die(err)
	}

	path := u.Path
	if path == "" || path[0] != '/' {
		path = "/" + path
	}

	// Re-read the chain through the query parser rather than trusting the raw
	// string: it is what the server will do with the URL it receives, so any
	// percent-decoding has to happen on both sides or the MACs will not match.
	sig, err := checker.Sign(httpserver.SignedMessage(path, u.Query().Get("x-oss-process")))
	if err != nil {
		die(err)
	}

	fmt.Println(sig)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "sign:", err)
	os.Exit(1)
}
