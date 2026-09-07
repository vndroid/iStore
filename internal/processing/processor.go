package processing

import (
	"github.com/kane/istore/internal/auximageprovider"
	"github.com/kane/istore/internal/security"
)

// Processor is responsible for processing images according to the given configuration.
type Processor struct {
	config            *Config
	securityChecker   *security.Checker
	watermarkProvider auximageprovider.Provider
}

// New creates a new Processor instance with the given configuration and watermark provider
func New(
	config *Config,
	securityChecker *security.Checker,
	watermark auximageprovider.Provider,
) (*Processor, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &Processor{
		config:            config,
		securityChecker:   securityChecker,
		watermarkProvider: watermark,
	}, nil
}
