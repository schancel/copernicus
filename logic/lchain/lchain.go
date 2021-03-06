package lchain

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/copernet/copernicus/conf"
	"github.com/copernet/copernicus/errcode"
	"github.com/copernet/copernicus/logic/lmempool"
	"github.com/copernet/copernicus/model"
	"github.com/copernet/copernicus/model/block"
	"github.com/copernet/copernicus/model/blockindex"
	"github.com/copernet/copernicus/model/chain"
	"github.com/copernet/copernicus/model/mempool"
	"github.com/copernet/copernicus/persist"
	"github.com/copernet/copernicus/util"

	"github.com/copernet/copernicus/log"
	"github.com/copernet/copernicus/logic/ltx"
	"github.com/copernet/copernicus/logic/lundo"

	"github.com/copernet/copernicus/model/undo"
	"github.com/copernet/copernicus/model/utxo"
	"github.com/copernet/copernicus/persist/disk"

	"github.com/copernet/copernicus/logic/lblock"
	mchain "github.com/copernet/copernicus/model/chain"
	"github.com/copernet/copernicus/model/consensus"
	"github.com/copernet/copernicus/model/pow"
	"github.com/copernet/copernicus/persist/db"
)

// IsInitialBlockDownload Check whether we are doing an initial block download
// (synchronizing from disk or network)
func IsInitialBlockDownload() bool {
	return persist.Reindex || !chain.GetInstance().IsAlmostSynced()
}

func ConnectBlock(pblock *block.Block, pindex *blockindex.BlockIndex, view *utxo.CoinsMap, fJustCheck bool) error {
	gChain := chain.GetInstance()
	start := time.Now()
	params := gChain.GetParams()
	// Check it again in case a previous version let a bad lblock in
	if err := lblock.CheckBlock(pblock, true, true); err != nil {
		return err
	}

	// Verify that the view's current state corresponds to the previous lblock
	var hashPrevBlock *util.Hash
	if pindex.Prev == nil {
		hashPrevBlock = &util.Hash{}
	} else {
		hashPrevBlock = pindex.Prev.GetBlockHash()
	}
	gUtxo := utxo.GetUtxoCacheInstance()
	bestHash, _ := gUtxo.GetBestBlock()
	log.Debug("bestHash = %s, hashPrevBloc = %s", bestHash, hashPrevBlock)
	if !hashPrevBlock.IsEqual(&bestHash) {
		log.Debug("will panic in ConnectBlock()")
		panic(fmt.Sprintf("error: hashPrevBlock:%s not equal view.GetBestBlock():%s", hashPrevBlock, bestHash))
	}

	// Special case for the genesis lblock, skipping connection of its
	// transactions (its coinbase is unspendable)
	blockHash := pblock.GetHash()
	if blockHash.IsEqual(params.GenesisHash) {
		return nil
	}

	fScriptChecks := true
	if chain.HashAssumeValid != util.HashZero {
		// We've been configured with the hash of a block which has been
		// externally verified to have a valid history. A suitable default value
		// is included with the software and updated from time to time. Because
		// validity relative to a piece of software is an objective fact these
		// defaults can be easily reviewed. This setting doesn't force the
		// selection of any particular chain but makes validating some faster by
		// effectively caching the result of part of the verification.
		if bi := gChain.FindBlockIndex(chain.HashAssumeValid); bi != nil {
			minimumChainWork := pow.MiniChainWork()
			pindexBestHeader := gChain.GetIndexBestHeader()
			if bi.GetAncestor(pindex.Height) == pindex &&
				pindexBestHeader.GetAncestor(pindex.Height) == pindex &&
				pindexBestHeader.ChainWork.Cmp(&minimumChainWork) >= 0 {
				// This block is a member of the assumed verified chain and an
				// ancestor of the best header. The equivalent time check
				// discourages hashpower from extorting the network via DOS
				// attack into accepting an invalid block through telling users
				// they must manually set assumevalid. Requiring a software
				// change or burying the invalid block, regardless of the
				// setting, makes it hard to hide the implication of the demand.
				// This also avoids having release candidates that are hardly
				// doing any signature verification at all in testing without
				// having to artificially set the default assumed verified block
				// further back. The test against nMinimumChainWork prevents the
				// skipping when denied access to any chain at least as good as
				// the expected chain.
				fScriptChecks = (pow.GetBlockProofEquivalentTime(
					pindexBestHeader, pindex, pindexBestHeader, params)) <= 60*60*24*7*2
			}
		}
	}

	time1 := time.Now()
	gPersist := persist.GetInstance()
	gPersist.GlobalTimeCheck += time1.Sub(start)
	log.Print("bench", "debug", " - Sanity checks: current %v [total %v]",
		time1.Sub(start), gPersist.GlobalTimeCheck)

	// Do not allow blocks that contain transactions which 'overwrite' older
	// transactions, unless those are already completely spent. If such
	// overwrites are allowed, coinbases and transactions depending upon those
	// can be duplicated to remove the ability to spend the first instance --
	// even after being sent to another address. See BIP30 and
	// http://r6.ca/blog/20120206T005236Z.html for more information. This logic
	// is not necessary for memory pool transactions, as AcceptToMemoryPool
	// already refuses previously-known transaction ids entirely. This rule was
	// originally applied to all blocks with a timestamp after March 15, 2012,
	// 0:00 UTC. Now that the whole chain is irreversibly beyond that time it is
	// applied to all blocks except the two in the chain that violate it. This
	// prevents exploiting the issue against nodes during their initial block
	// download.
	//zHash := util.HashZero
	//fEnforceBIP30 := (!blockHash.IsEqual(&zHash)) ||
	//	!((pindex.Height == 91842 &&
	//		blockHash.IsEqual(util.HashFromString("0x00000000000a4d0a398161ffc163c503763b1f4360639393e0e4c8e300e0caec"))) ||
	//		(pindex.Height == 91880 &&
	//			blockHash.IsEqual(util.HashFromString("0x00000000000743f190a18c5577a3c2d2a1f610ae9601ac046a38084ccb7cd721"))))
	bip30Enable := !((pindex.Height == 91842 && blockHash.IsEqual(util.HashFromString("00000000000a4d0a398161ffc163c503763b1f4360639393e0e4c8e300e0caec"))) ||
		(pindex.Height == 91880 && blockHash.IsEqual(util.HashFromString("00000000000743f190a18c5577a3c2d2a1f610ae9601ac046a38084ccb7cd721"))))

	// Once BIP34 activated it was not possible to create new duplicate
	// coinBases and thus other than starting with the 2 existing duplicate
	// coinBase pairs, not possible to create overwriting txs. But by the time
	// BIP34 activated, in each of the existing pairs the duplicate coinBase had
	// overwritten the first before the first had been spent. Since those
	// coinBases are sufficiently buried its no longer possible to create
	// further duplicate transactions descending from the known pairs either. If
	// we're on the known chain at height greater than where BIP34 activated, we
	// can save the db accesses needed for the BIP30 check.
	pindexBIP34height := pindex.Prev.GetAncestor(params.BIP34Height)
	// Only continue to enforce if we're below BIP34 activation height or the
	// block hash at that height doesn't correspond.
	bip34Enable := pindexBIP34height != nil && pindexBIP34height.GetBlockHash().IsEqual(&params.BIP34Hash)
	bip30Enable = bip30Enable && !bip34Enable

	lockTimeFlags := 0
	if pindex.Height >= gChain.GetParams().CSVHeight {
		lockTimeFlags |= consensus.LocktimeVerifySequence
	}

	flags := lblock.GetBlockScriptFlags(pindex.Prev)
	log.Debug("Connect Block: %s, height: %d, flags: %d", pindex.GetBlockHash().String(), pindex.Height, flags)
	blockSubSidy := model.GetBlockSubsidy(pindex.Height, params)
	time2 := time.Now()
	gPersist.GlobalTimeForks += time2.Sub(time1)
	log.Print("bench", "debug", " - Fork checks: current %v [total %v]",
		time2.Sub(time1), gPersist.GlobalTimeForks)

	maxSigOps, errSig := consensus.GetMaxBlockSigOpsCount(uint64(pblock.EncodeSize()))
	if errSig != nil {
		return errSig
	}

	coinsMap, blockUndo, err := ltx.ApplyBlockTransactions(pblock.Txs, bip30Enable, flags,
		fScriptChecks, blockSubSidy, pindex.Height, maxSigOps, uint32(lockTimeFlags), pindex)
	if err != nil {
		return err
	}

	undoPos := pindex.GetUndoPos()
	// Write undo information to disk
	if !fJustCheck {
		if undoPos.IsNull() || !pindex.IsValid(blockindex.BlockValidScripts) {
			if undoPos.IsNull() {
				pos := block.NewDiskBlockPos(pindex.File, 0)
				//blockUndo size + hash size + 4bytes len
				if err := disk.FindUndoPos(pindex.File, pos, blockUndo.SerializeSize()+36); err != nil {
					return err
				}
				if err := disk.UndoWriteToDisk(blockUndo, pos, *pindex.Prev.GetBlockHash(), params.BitcoinNet); err != nil {
					return err
				}

				// update nUndoPos in block index
				pindex.UndoPos = pos.Pos
				pindex.AddStatus(blockindex.BlockHaveUndo)
			}
			pindex.RaiseValidity(blockindex.BlockValidScripts)
			gPersist.AddDirtyBlockIndex(pindex)
		}
		// add this block to the view's block chain
		*view = *coinsMap
	}

	// If we just activated the replay protection with that block, it means
	// transaction in the mempool are now invalid. As a result, we need to clear the mempool.
	if pindex.IsReplayProtectionJustEnabled() {
		mempool.InitMempool()
	}

	log.Debug("Connect block heigh:%d, hash:%s, txs: %d", pindex.Height, blockHash, len(pblock.Txs))
	return nil
}

//InvalidBlockFound the found block is invalid
func InvalidBlockFound(pindex *blockindex.BlockIndex) {
	pindex.AddStatus(blockindex.BlockFailed)
	mchain.GetInstance().RemoveFromBranch(pindex)
	persist.GetInstance().AddDirtyBlockIndex(pindex)
}

func InvalidBlockParentFound(pindex *blockindex.BlockIndex) {
	pindex.AddStatus(blockindex.BlockFailedParent)
	mchain.GetInstance().RemoveFromBranch(pindex)
	persist.GetInstance().AddDirtyBlockIndex(pindex)
}

type connectTrace map[*blockindex.BlockIndex]*block.Block

// ConnectTip Connect a new block to chainActive. block is either nullptr or a pointer to
// a CBlock corresponding to indexNew, to bypass loading it again from disk.
// The block is always added to connectTrace (either after loading from disk or
// by copying block) - if that is not intended, care must be taken to remove
// the last entry in blocksConnected in case of failure.
func ConnectTip(pIndexNew *blockindex.BlockIndex,
	block *block.Block, connTrace connectTrace) error {
	gChain := chain.GetInstance()
	tip := gChain.Tip()

	if pIndexNew.Prev != tip {
		log.Error("error: try to connect to inactive chain!!!")
		panic("error: try to connect to inactive chain!!!")
	}
	// Read block from disk.
	nTime1 := util.GetTimeMicroSec()
	if block == nil {
		blockNew, err := disk.ReadBlockFromDisk(pIndexNew, gChain.GetParams())
		if !err || blockNew == nil {
			log.Error("error: FailedToReadBlock: %v", err)
			return errcode.New(errcode.FailedToReadBlock)
		}
		connTrace[pIndexNew] = blockNew
		block = blockNew
	} else {
		connTrace[pIndexNew] = block
	}
	blockConnecting := block
	indexHash := blockConnecting.GetHash()
	// Apply the block atomically to the chain state.
	nTime2 := util.GetTimeMicroSec()
	gPersist := persist.GetInstance()
	gPersist.GlobalTimeReadFromDisk += nTime2 - nTime1
	log.Info("Load block from disk: %d us total: [%.6f s]\n", nTime2-nTime1, float64(gPersist.GlobalTimeReadFromDisk)*0.000001)

	view := utxo.NewEmptyCoinsMap()
	err := ConnectBlock(blockConnecting, pIndexNew, view, false)
	if err != nil {
		InvalidBlockFound(pIndexNew)
		log.Error("ConnectTip(): ConnectBlock %s failed, err:%v", indexHash, err)
		return err
	}
	nTime3 := util.GetTimeMicroSec()
	gPersist.GlobalTimeConnectTotal += nTime3 - nTime2
	log.Debug("Connect total: %d us [%.2fs]\n", nTime3-nTime2, float64(gPersist.GlobalTimeConnectTotal)*0.000001)

	//flushed := view.Flush(indexHash)
	err = utxo.GetUtxoCacheInstance().UpdateCoins(view, &indexHash)
	if err != nil {
		panic("here should be true when view flush state")
	}
	nTime4 := util.GetTimeMicroSec()
	gPersist.GlobalTimeFlush += nTime4 - nTime3
	log.Print("bench", "debug", " - Flush: %d us [%.2fs]\n",
		nTime4-nTime3, float64(gPersist.GlobalTimeFlush)*0.000001)

	// Write the chain state to disk, if necessary.
	if err := disk.FlushStateToDisk(disk.FlushStateAlways, 0); err != nil {
		return err
	}
	if pIndexNew.Height >= conf.Cfg.Chain.UtxoHashStartHeight && pIndexNew.Height < conf.Cfg.Chain.UtxoHashEndHeight {
		cdb := utxo.GetUtxoCacheInstance().(*utxo.CoinsLruCache).GetCoinsDB()
		besthash, err := cdb.GetBestBlock()
		if err != nil {
			log.Debug("in utxostats, GetBestBlock(), failed=%v\n", err)
			return err
		}
		var stat stat
		stat.bestblock = *besthash
		stat.height = int(mchain.GetInstance().FindBlockIndex(*besthash).Height)
		iter := cdb.GetDBW().Iterator(nil)
		iter.Seek([]byte{db.DbCoin})
		taskControl.StartLogTask()
		taskControl.StartUtxoTask()
		taskControl.PushUtxoTask(utxoTaskArg{iter, &stat})
	}
	nTime5 := util.GetTimeMicroSec()
	gPersist.GlobalTimeChainState += nTime5 - nTime4
	log.Print("bench", "debug", " - Writing chainstate: %.2fms [%.2fs]\n",
		float64(nTime5-nTime4)*0.001, float64(gPersist.GlobalTimeChainState)*0.000001)

	// Remove conflicting transactions from the mempool.;
	mempool.GetInstance().RemoveTxSelf(blockConnecting.Txs)
	// Update chainActive & related variables.
	UpdateTip(pIndexNew)
	nTime6 := util.GetTimeMicroSec()
	gPersist.GlobalTimePostConnect += nTime6 - nTime5
	gPersist.GlobalTimeTotal += nTime6 - nTime1
	log.Print("bench", "debug", " - Connect postprocess: %.2fms [%.2fs]\n",
		float64(nTime6-nTime5)*0.001, float64(gPersist.GlobalTimePostConnect)*0.000001)
	log.Print("bench", "debug", " - Connect block: %.2fms [%.2fs]\n",
		float64(nTime6-nTime1)*0.001, float64(gPersist.GlobalTimeTotal)*0.000001)

	return nil
}

// DisconnectTip Disconnect chainActive's tip. You probably want to call
// mempool.removeForReorg and manually re-limit mempool size after this, with
// cs_main held.
func DisconnectTip(fBare bool) error {
	gChain := chain.GetInstance()
	tip := gChain.Tip()
	if tip == nil {
		panic("the chain tip element should not equal nil")
	}
	log.Warn("DisconnectTip block(%s)", tip.GetBlockHash())
	// Read block from disk.
	blk, ret := disk.ReadBlockFromDisk(tip, gChain.GetParams())
	if !ret {
		log.Error("DisconnectTip: read block from disk failed, the block is:%+v ", blk)
		return errcode.New(errcode.FailedToReadBlock)
	}

	// Apply the block atomically to the chain state.
	nStart := time.Now().UnixNano()
	{
		view := utxo.NewEmptyCoinsMap()

		if DisconnectBlock(blk, tip, view) != undo.DisconnectOk {
			log.Error(fmt.Sprintf("DisconnectTip(): DisconnectBlock %s failed ", tip.GetBlockHash()))
			return errcode.New(errcode.DisconnectTipUndoFailed)
		}
		//flushed := view.Flush(blk.Header.HashPrevBlock)
		err := utxo.GetUtxoCacheInstance().UpdateCoins(view, &blk.Header.HashPrevBlock)
		if err != nil {
			panic("view flush error !!!")
		}
		utxo.GetUtxoCacheInstance().Flush()
	}
	// replace implement with log.Print(in C++).
	log.Info("bench-debug - Disconnect block : %.2fms\n",
		float64(time.Now().UnixNano()-nStart)*0.001)

	// Write the chain state to disk, if necessary.
	if err := disk.FlushStateToDisk(disk.FlushStateIfNeeded, 0); err != nil {
		return err
	}

	// If this block was deactivating the replay protection, then we need to
	// remove transactions that are replay protected from the mempool. There is
	// no easy way to do this so we'll just discard the whole mempool and then
	// add the transaction of the block we just disconnected back.
	//
	// If we are deactivating Magnetic anomaly, we want to make sure we do not
	// have transactions in the mempool that use newly introduced opcodes. As a
	// result, we also cleanup the mempool.
	if tip.IsReplayProtectionJustEnabled() || tip.IsMagneticAnomalyJustEnabled() {
		mempool.InitMempool()
	}

	UpdateTip(tip.Prev)

	if !fBare {
		// Resurrect mempool transactions from the disconnected block.
		for _, tx := range blk.Txs {
			// ignore validation errors in resurrected transactions
			if tx.IsCoinBase() {
				mempool.GetInstance().RemoveTxRecursive(tx, mempool.REORG)
			} else {
				e := lmempool.AcceptTxToMemPool(tx)
				if e != nil {
					mempool.GetInstance().RemoveTxRecursive(tx, mempool.REORG)
				}
			}
		}
	}
	gChain.SendNotification(chain.NTBlockDisconnected, blk)
	return nil
}

// UpdateTip Update chainActive and related internal data structures.
func UpdateTip(pindexNew *blockindex.BlockIndex) {
	gChain := chain.GetInstance()
	gChain.SetTip(pindexNew)
	param := gChain.GetParams()
	warningMessages := make([]string, 0)
	txdata := param.TxData()
	tip := chain.GetInstance().Tip()
	utxoTip := utxo.GetUtxoCacheInstance()
	log.Info("new best=%s height=%d version=0x%08x work=%s tx=%d "+
		"date='%s' progress=%f memory=%d(cache=%d)", tip.GetBlockHash(),
		tip.Height, tip.Header.Version,
		tip.ChainWork.String(), tip.ChainTxCount,
		time.Unix(int64(tip.Header.Time), 0).String(),
		GuessVerificationProgress(txdata, tip),
		utxoTip.DynamicMemoryUsage(), utxoTip.GetCacheSize())
	if len(warningMessages) != 0 {
		log.Info("waring= %s", strings.Join(warningMessages, ","))
	}
}

// GuessVerificationProgress Guess how far we are in the verification process at the given block index
func GuessVerificationProgress(data *model.ChainTxData, index *blockindex.BlockIndex) float64 {
	if index == nil {
		return float64(0)
	}

	now := time.Now()

	var txTotal float64
	// todo confirm time precise
	if int64(index.ChainTxCount) <= data.TxCount {
		txTotal = float64(data.TxCount) + (now.Sub(data.Time).Seconds())*data.TxRate
	} else {
		txTotal = float64(index.ChainTxCount) + float64(now.Unix()-int64(index.GetBlockTime()))*data.TxRate
	}
	return float64(index.ChainTxCount) / txTotal
}

func DisconnectBlock(pblock *block.Block, pindex *blockindex.BlockIndex, view *utxo.CoinsMap) undo.DisconnectResult {
	hashA := pindex.GetBlockHash()
	hashB, _ := utxo.GetUtxoCacheInstance().GetBestBlock()
	if !bytes.Equal(hashA[:], hashB[:]) {
		panic("the two hash should be equal ...")
	}
	pos := pindex.GetUndoPos()
	if pos.IsNull() {
		log.Error("DisconnectBlock(): no undo data available.")
		return undo.DisconnectFailed
	}
	blockUndo, ret := disk.UndoReadFromDisk(&pos, *pindex.Prev.GetBlockHash())
	if !ret {
		log.Error("DisconnectBlock(): reading undo data failed, pos is: %s, block undo is: %+v", pos.String(), blockUndo)
		return undo.DisconnectFailed
	}

	return lundo.ApplyBlockUndo(blockUndo, pblock, view, pindex.Height)
}

func InitGenesisChain() error {
	gChain := chain.GetInstance()
	if gChain.Genesis() != nil {
		return nil
	}

	// Write genesisblock to disk
	bl := gChain.GetParams().GenesisBlock
	pos := block.NewDiskBlockPos(0, 0)
	flag := disk.FindBlockPos(pos, uint32(bl.SerializeSize()+4), 0, uint64(bl.GetBlockHeader().Time), false)
	if !flag {
		log.Error("InitChain.WriteBlockToDisk():FindBlockPos failed")
		return errcode.ProjectError{Code: 2000}
	}
	flag = disk.WriteBlockToDisk(bl, pos)
	if !flag {
		log.Error("InitChain.WriteBlockToDisk():WriteBlockToDisk failed")
		return errcode.ProjectError{Code: 2001}
	}

	// Add genesis block index to DB and make the Chain
	bIndex := blockindex.NewBlockIndex(&bl.Header)
	bIndex.Height = 0
	err := gChain.AddToIndexMap(bIndex)
	if err != nil {
		return err
	}
	lblock.ReceivedBlockTransactions(bl, bIndex, pos)
	gChain.SetTip(bIndex)

	// Set bestblockhash to DB
	coinsMap := utxo.NewEmptyCoinsMap()
	//coinsMap, _, _ := ltx.ApplyGeniusBlockTransactions(bl.Txs)
	bestHash := bIndex.GetBlockHash()
	utxo.GetUtxoCacheInstance().UpdateCoins(coinsMap, bestHash)

	err = disk.FlushStateToDisk(disk.FlushStateAlways, 0)

	return err
}
