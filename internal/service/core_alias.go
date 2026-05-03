package service

import "longtradego/internal/core"

type AppContext = core.AppContext
type CommandFileLogger = core.CommandFileLogger

const (
	commandLogDir     = core.CommandLogDir
	commandLogMaxSize = core.CommandLogMaxSize
)

var (
	ParseSymbols              = core.ParseSymbols
	NormalizeArgs             = core.NormalizeArgs
	InferCommandMetadata      = core.InferCommandMetadata
	FormatCommandLine         = core.FormatCommandLine
	NewAppContext             = core.NewAppContext
	writeFileAtomic           = core.WriteFileAtomic
	appendLogLineWithRotation = core.AppendLogLineWithRotation
	getEnvFirst               = core.GetEnvFirst
)
