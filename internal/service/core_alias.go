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
	defaultConfigDir          = core.DefaultConfigDir
	defaultDataDir            = core.DefaultDataDir
	defaultLogDir             = core.DefaultLogDir
	resolveConfigPath         = core.ResolveConfigPath
	resolveDataPath           = core.ResolveDataPath
	resolveLogPath            = core.ResolveLogPath
	defaultEnvFilePath        = core.DefaultEnvFilePath
	legacyRepoEnvFilePath     = core.LegacyRepoEnvFilePath
	useRepoRelativeLayout     = core.UseRepoRelativeLayout
)
