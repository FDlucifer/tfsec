package externalscan

import "github.com/tfsec/tfsec/internal/app/tfsec/scanner"

type Option func(e *ExternalScanner)

func OptionIncludePassed() Option {
	return func(e *ExternalScanner) {
		e.internalOptions = append(e.internalOptions, scanner.OptionIncludePassed())
	}
}
