package engine

import (
	"musix/engine/internal/config"
)

type Config = config.Config

const (
	DefaultDomain         = config.DefaultDomain
	DefaultLoopbackHost   = config.DefaultLoopbackHost
	DefaultChunkSize       = config.DefaultChunkSize
	InitialSmallChunkSize = config.InitialSmallChunkSize
	FullChunkSize          = config.FullChunkSize
	MinReadAheadBytes      = config.MinReadAheadBytes
	MaxReadAheadBytes      = config.MaxReadAheadBytes
	ArtworkDirName        = config.ArtworkDirName
)

