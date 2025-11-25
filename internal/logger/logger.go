package logger

import (
	"os"
	"strings"
	"time"

	"github.com/bilal/switchify-agent/internal/config"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func Init(lcfg config.LoggingConfig) {
	// level
	level := strings.ToLower(lcfg.Level)
	levelVal := zerolog.InfoLevel
	switch level {
	case "debug":
		levelVal = zerolog.DebugLevel
	case "info":
		levelVal = zerolog.InfoLevel
	case "warn", "warning":
		levelVal = zerolog.WarnLevel
	case "error":
		levelVal = zerolog.ErrorLevel
	}
	zerolog.SetGlobalLevel(levelVal)

	// format
	if strings.ToLower(lcfg.Format) == "console" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	} else {
		// default json
		log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	}
}
