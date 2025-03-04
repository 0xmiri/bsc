// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *StateProcessor {
	return &StateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

type MethSimulation struct {
	header          *types.Header
	block           *types.Block
	blockHash       common.Hash
	blockNumber     *big.Int
	gasPool         *GasPool
	evm             *vm.EVM
	signer          types.Signer
	posa            consensus.PoSA
	isPoSA          bool
	bloomProcessors *AsyncReceiptBloomGenerator
	systemTxs       []*types.Transaction
	commonTxs       []*types.Transaction
	receipts        []*types.Receipt
	usedGas         *uint64
}

func (p *StateProcessor) PrepareEnv(block *types.Block, statedb *state.StateDB, cfg vm.Config) *MethSimulation {
	var (
		header      = block.Header()
		blockNumber = block.Number()
		gp          = new(GasPool).AddGas(block.GasLimit())
	)

	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	// Handle upgrade build-in system contract code
	lastBlock := p.bc.GetBlockByHash(block.ParentHash())
	if lastBlock == nil {
		return nil
	}

	systemcontracts.UpgradeBuildInSystemContract(p.config, blockNumber, lastBlock.Time(), block.Time(), statedb)

	var (
		context = NewEVMBlockContext(header, p.bc, nil)
		vmenv   = vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
		signer  = types.MakeSigner(p.config, header.Number, header.Time)
		txNum   = len(block.Transactions())
	)
	// Iterate over and process the individual transactions
	posa, isPoSA := p.engine.(consensus.PoSA)

	// initialise bloom processors
	bloomProcessors := NewAsyncReceiptBloomGenerator(txNum)
	statedb.MarkFullProcessed()

	systemTxs := make([]*types.Transaction, 0, 2)
	commonTxs := make([]*types.Transaction, 0, txNum)
	var receipts = make([]*types.Receipt, 0)
	var usedGas uint64 = 0

	meth := MethSimulation{
		block:           block,
		header:          block.Header(),
		blockHash:       block.Hash(),
		blockNumber:     block.Number(),
		gasPool:         gp,
		evm:             vmenv,
		signer:          signer,
		posa:            posa,
		isPoSA:          isPoSA,
		bloomProcessors: bloomProcessors,
		systemTxs:       systemTxs,
		commonTxs:       commonTxs,
		receipts:        receipts,
		usedGas:         &usedGas,
	}

	return &meth
}

func (p *StateProcessor) ProcessTx(meth *MethSimulation, statedb *state.StateDB, i int) (*state.StateDB, *types.Receipt, []*types.Log, uint64, *int, error) {
	//wtf
	block, header, blockHash, blockNumber, gp, vmenv, signer, posa, isPoSA, bloomProcessors, systemTxs, commonTxs, receipts, usedGas := meth.block, meth.header,
		meth.blockHash, meth.blockNumber, meth.gasPool, meth.evm, meth.signer, meth.posa, meth.isPoSA, meth.bloomProcessors, meth.systemTxs, meth.commonTxs, meth.receipts, meth.usedGas

	var nextTxIndex = i

	if i == meth.block.Transactions().Len() {
		return nil, nil, nil, 0, nil, errors.New("end of block")
	}

	nextTxIndex += 1

	tx := meth.block.Transactions()[i]
	if isPoSA {
		if isSystemTx, err := posa.IsSystemTransaction(tx, block.Header()); err != nil {
			bloomProcessors.Close()
			return statedb, nil, nil, 0, &i, err
		} else if isSystemTx {
			meth.systemTxs = append(systemTxs, tx)
			return statedb, nil, nil, 0, &nextTxIndex, nil
		}
	}

	msg, err := TransactionToMessage(tx, signer, header.BaseFee)
	if err != nil {
		bloomProcessors.Close()
		return statedb, nil, nil, 0, &i, err
	}
	statedb.SetTxContext(tx.Hash(), i)

	receipt, err := applyTransaction(msg, p.config, gp, statedb, blockNumber, blockHash, tx, usedGas, vmenv, bloomProcessors)
	if err != nil {
		bloomProcessors.Close()
		return statedb, nil, nil, 0, &i, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
	}

	meth.commonTxs = append(commonTxs, tx)
	meth.receipts = append(receipts, receipt)

	return statedb, receipt, receipt.Logs, receipt.GasUsed, &nextTxIndex, nil

}

func (p *StateProcessor) Commit(meth *MethSimulation, statedb *state.StateDB) (*state.StateDB, []*types.Receipt, []*types.Log, uint64, error) {
	block, header, receipts, systemTxs, usedGas, commonTxs, bloomProcessors := meth.block, meth.header, meth.receipts, meth.systemTxs, meth.usedGas, meth.commonTxs, meth.bloomProcessors

	bloomProcessors.Close()

	// Fail if Shanghai not enabled and len(withdrawals) is non-zero.
	withdrawals := block.Withdrawals()
	if len(withdrawals) > 0 && !p.config.IsShanghai(block.Number(), block.Time()) {
		return statedb, nil, nil, 0, errors.New("withdrawals before shanghai")
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	err := p.engine.Finalize(p.bc, header, statedb, &commonTxs, block.Uncles(), withdrawals, &receipts, &systemTxs, usedGas)

	var allLogs []*types.Log
	for _, receipt := range receipts {
		allLogs = append(allLogs, receipt.Logs...)
	}

	if err != nil {
		return statedb, nil, nil, *usedGas, err
	}

	return statedb, receipts, allLogs, *usedGas, nil
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (*state.StateDB, types.Receipts, []*types.Log, uint64, error) {
	var (
		usedGas     = new(uint64)
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		gp          = new(GasPool).AddGas(block.GasLimit())
	)

	var receipts = make([]*types.Receipt, 0)
	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	// Handle upgrade build-in system contract code
	lastBlock := p.bc.GetBlockByHash(block.ParentHash())
	if lastBlock == nil {
		return statedb, nil, nil, 0, fmt.Errorf("could not get parent block")
	}
	systemcontracts.UpgradeBuildInSystemContract(p.config, blockNumber, lastBlock.Time(), block.Time(), statedb)

	var (
		context = NewEVMBlockContext(header, p.bc, nil)
		vmenv   = vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
		signer  = types.MakeSigner(p.config, header.Number, header.Time)
		txNum   = len(block.Transactions())
	)
	// Iterate over and process the individual transactions
	posa, isPoSA := p.engine.(consensus.PoSA)
	commonTxs := make([]*types.Transaction, 0, txNum)

	// initialise bloom processors
	bloomProcessors := NewAsyncReceiptBloomGenerator(txNum)
	statedb.MarkFullProcessed()

	// usually do have two tx, one for validator set contract, another for system reward contract.
	systemTxs := make([]*types.Transaction, 0, 2)

	for i, tx := range block.Transactions() {
		if isPoSA {
			if isSystemTx, err := posa.IsSystemTransaction(tx, block.Header()); err != nil {
				bloomProcessors.Close()
				return statedb, nil, nil, 0, err
			} else if isSystemTx {
				systemTxs = append(systemTxs, tx)
				continue
			}
		}

		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			bloomProcessors.Close()
			return statedb, nil, nil, 0, err
		}
		statedb.SetTxContext(tx.Hash(), i)

		receipt, err := applyTransaction(msg, p.config, gp, statedb, blockNumber, blockHash, tx, usedGas, vmenv, bloomProcessors)
		if err != nil {
			bloomProcessors.Close()
			return statedb, nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		commonTxs = append(commonTxs, tx)
		receipts = append(receipts, receipt)
	}
	bloomProcessors.Close()

	// Fail if Shanghai not enabled and len(withdrawals) is non-zero.
	withdrawals := block.Withdrawals()
	if len(withdrawals) > 0 && !p.config.IsShanghai(block.Number(), block.Time()) {
		return nil, nil, nil, 0, errors.New("withdrawals before shanghai")
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	err := p.engine.Finalize(p.bc, header, statedb, &commonTxs, block.Uncles(), withdrawals, &receipts, &systemTxs, usedGas)
	if err != nil {
		return statedb, receipts, allLogs, *usedGas, err
	}
	for _, receipt := range receipts {
		allLogs = append(allLogs, receipt.Logs...)
	}

	return statedb, receipts, allLogs, *usedGas, nil
}

func applyTransaction(msg *Message, config *params.ChainConfig, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, tx *types.Transaction, usedGas *uint64, evm *vm.EVM, receiptProcessors ...ReceiptProcessor) (*types.Receipt, error) {
	// Create a new context to be used in the EVM environment.
	txContext := NewEVMTxContext(msg)
	evm.Reset(txContext, statedb)

	// Apply the transaction to the current state (included in the env).
	result, err := ApplyMessage(evm, msg, gp)
	if err != nil {
		return nil, err
	}

	// Update the state with pending changes.
	var root []byte
	if config.IsByzantium(blockNumber) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(config.IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: *usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	// If the transaction created a contract, store the creation address in the receipt.
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash)
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	for _, receiptProcessor := range receiptProcessors {
		receiptProcessor.Apply(receipt)
	}
	return receipt, err
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(config *params.ChainConfig, bc ChainContext, author *common.Address, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64, cfg vm.Config, receiptProcessors ...ReceiptProcessor) (*types.Receipt, error) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(config, header.Number, header.Time), header.BaseFee)
	if err != nil {
		return nil, err
	}
	// Create a new context to be used in the EVM environment
	blockContext := NewEVMBlockContext(header, bc, author)
	vmenv := vm.NewEVM(blockContext, vm.TxContext{BlobHashes: tx.BlobHashes()}, statedb, config, cfg)
	defer func() {
		ite := vmenv.Interpreter()
		vm.EVMInterpreterPool.Put(ite)
		vm.EvmPool.Put(vmenv)
	}()
	return applyTransaction(msg, config, gp, statedb, header.Number, header.Hash(), tx, usedGas, vmenv, receiptProcessors...)
}
