//go:build wasm
// +build wasm

/*
 * Flow Emulator
 *
 * Copyright 2019-2022 Dapper Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package server

import (
	"fmt"
	"time"

	"github.com/onflow/cadence"
	"github.com/onflow/flow-emulator/wasmhttp"
	"github.com/onflow/flow-go-sdk/crypto"
	"github.com/onflow/flow-go/engine/access/rest"
	"github.com/onflow/flow-go/fvm"
	"github.com/onflow/flow-go/model/flow"
	"github.com/psiemens/graceland"
	"github.com/rs/zerolog"
	"github.com/sirupsen/logrus"

	emulator "github.com/onflow/flow-emulator"
	"github.com/onflow/flow-emulator/server/backend"
	"github.com/onflow/flow-emulator/storage"
)

// EmulatorServer is a local server that runs a Flow Emulator instance.
//
// The server wraps an emulated blockchain instance with the Access API gRPC handlers.
type EmulatorServer struct {
	logger     *logrus.Logger
	config     *Config
	backend    *backend.Backend
	group      *graceland.Group
	storage    graceland.Routine
	rest       graceland.Routine
	blocks     graceland.Routine
	blockchain *emulator.Blockchain
}
type HTTPHeader struct {
	Key   string
	Value string
}

const (
	defaultGRPCPort               = 3569
	defaultRESTPort               = 8888
	defaultAdminPort              = 8080
	defaultLivenessCheckTolerance = time.Second
	defaultDBGCInterval           = time.Minute * 5
	defaultDBGCRatio              = 0.5
)

// Config is the configuration for an emulator server.
type Config struct {
	GRPCPort                     int
	GRPCDebug                    bool
	AdminPort                    int
	RESTPort                     int
	RESTDebug                    bool
	HTTPHeaders                  []HTTPHeader
	BlockTime                    time.Duration
	ServicePublicKey             crypto.PublicKey
	ServicePrivateKey            crypto.PrivateKey
	ServiceKeySigAlgo            crypto.SignatureAlgorithm
	ServiceKeyHashAlgo           crypto.HashAlgorithm
	SimpleAddresses              bool
	GenesisTokenSupply           cadence.UFix64
	TransactionExpiry            uint
	StorageLimitEnabled          bool
	MinimumStorageReservation    cadence.UFix64
	StorageMBPerFLOW             cadence.UFix64
	TransactionFeesEnabled       bool
	TransactionValidationEnabled bool
	TransactionMaxGasLimit       uint64
	ScriptGasLimit               uint64
	Persist                      bool
	// DBPath is the path to the Badger database on disk.
	DBPath string
	// DBGCInterval is the time interval at which to garbage collect the Badger value log.
	DBGCInterval time.Duration
	// DBGCDiscardRatio is the ratio of space to reclaim during a Badger garbage collection run.
	DBGCDiscardRatio float64
	// LivenessCheckTolerance is the time interval in which the server must respond to liveness probes.
	LivenessCheckTolerance time.Duration
	// Whether to deploy some extra Flow contracts when emulator starts
	WithContracts bool
}

//logger hook for wasm
type WasmLogHook struct {
}

func (hook *WasmLogHook) Fire(entry *logrus.Entry) error {
	wasmhttp.Log(entry)
	return nil
}

func (hook *WasmLogHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// NewEmulatorServer creates a new instance of a Flow Emulator server.
func NewEmulatorServer(logger *logrus.Logger, conf *Config) *EmulatorServer {
	conf = sanitizeConfig(conf)

	store, err := configureStorage(logger, conf)
	if err != nil {
		logger.WithError(err).Error("❗  Failed to configure storage")
		return nil
	}

	blockchain, err := configureBlockchain(conf, store.Store())
	if err != nil {
		logger.WithError(err).Error("❗  Failed to configure emulated blockchain")
		return nil
	}

	chain := blockchain.GetChain()

	contracts := map[string]string{
		"FlowServiceAccount": chain.ServiceAddress().HexWithPrefix(),
		"FlowToken":          fvm.FlowTokenAddress(chain).HexWithPrefix(),
		"FungibleToken":      fvm.FungibleTokenAddress(chain).HexWithPrefix(),
		"FlowFees":           fvm.FlowFeesAddress(chain).HexWithPrefix(),
		"FlowStorageFees":    chain.ServiceAddress().HexWithPrefix(),
	}
	for contract, address := range contracts {
		logger.WithFields(logrus.Fields{contract: address}).Infof("📜  Flow contract")
	}

	if conf.WithContracts {
		deployments, err := deployContracts(blockchain)
		if err != nil {
			logger.WithError(err).Error("❗  Failed to deploy contracts")
		}

		for _, contract := range deployments {
			logger.WithFields(logrus.Fields{
				contract.name: fmt.Sprintf("0x%s", contract.address.Hex())}).Infof(contract.description)
		}
	}

	be := configureBackend(logger, conf, blockchain)

	server := &EmulatorServer{
		logger:     logger,
		config:     conf,
		backend:    be,
		storage:    store,
		blockchain: blockchain,
	}

	// only create blocks ticker if block time > 0
	if conf.BlockTime > 0 {
		server.blocks = NewBlocksTicker(be, conf.BlockTime)
	}

	return server
}

// Start starts the Flow Emulator server.
func (s *EmulatorServer) Start() {

	s.logger.
		WithField("port", s.config.RESTPort).
		Info("🌱  Starting REST API on wasm")

	srv, err := rest.NewServer(backend.NewAdapter(s.backend), "127.0.0.1:3333", zerolog.Logger{}, flow.Emulator.Chain())
	if err != nil {
		s.logger.WithError(err).Error("❗  Backend error")
	}

	s.logger.AddHook(&WasmLogHook{})
	wasmhttp.Serve(srv.Handler)

	err = s.storage.Start()

	if err != nil {
		s.logger.WithError(err).Error("❗  Server error")
	}

	for {
		time.Sleep(time.Second)
	}

}

func (s *EmulatorServer) Stop() {

	s.logger.Info("🛑  Server stopped")
}

func configureStorage(logger *logrus.Logger, conf *Config) (storage Storage, err error) {
	if conf.Persist {
		return NewBadgerStorage(logger, conf.DBPath, conf.DBGCInterval, conf.DBGCDiscardRatio)
	}

	return NewMemoryStorage(), nil
}

func configureBlockchain(conf *Config, store storage.Store) (*emulator.Blockchain, error) {
	options := []emulator.Option{
		emulator.WithStore(store),
		emulator.WithGenesisTokenSupply(conf.GenesisTokenSupply),
		emulator.WithSimpleAddresses(conf.SimpleAddresses),
		emulator.WithTransactionMaxGasLimit(conf.TransactionMaxGasLimit),
		emulator.WithScriptGasLimit(conf.ScriptGasLimit),
		emulator.WithTransactionExpiry(conf.TransactionExpiry),
		emulator.WithStorageLimitEnabled(conf.StorageLimitEnabled),
		emulator.WithMinimumStorageReservation(conf.MinimumStorageReservation),
		emulator.WithStorageMBPerFLOW(conf.StorageMBPerFLOW),
		emulator.WithTransactionFeesEnabled(conf.TransactionFeesEnabled),
		emulator.WithTransactionValidationEnabled(conf.TransactionValidationEnabled),
	}

	if conf.ServicePrivateKey != nil {
		options = append(
			options,
			emulator.WithServicePrivateKey(conf.ServicePrivateKey, conf.ServiceKeySigAlgo, conf.ServiceKeyHashAlgo),
		)
	} else if conf.ServicePublicKey != nil {
		options = append(
			options,
			emulator.WithServicePublicKey(conf.ServicePublicKey, conf.ServiceKeySigAlgo, conf.ServiceKeyHashAlgo),
		)
	}

	blockchain, err := emulator.NewBlockchain(options...)
	if err != nil {
		return nil, err
	}

	return blockchain, nil
}

func configureBackend(logger *logrus.Logger, conf *Config, blockchain *emulator.Blockchain) *backend.Backend {
	b := backend.New(logger, blockchain)

	if conf.BlockTime == 0 {
		b.EnableAutoMine()
	}

	return b
}

func sanitizeConfig(conf *Config) *Config {

	if conf.RESTPort == 0 {
		conf.RESTPort = defaultRESTPort
	}

	if conf.DBGCInterval == 0 {
		conf.DBGCInterval = defaultDBGCInterval
	}

	if conf.DBGCDiscardRatio == 0 {
		conf.DBGCDiscardRatio = defaultDBGCRatio
	}

	if conf.LivenessCheckTolerance == 0 {
		conf.LivenessCheckTolerance = defaultLivenessCheckTolerance
	}

	return conf
}
