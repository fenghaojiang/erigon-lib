/*
   Copyright 2021 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package state

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"path/filepath"

	"github.com/google/btree"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/crypto/sha3"

	"github.com/ledgerwatch/erigon-lib/commitment"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/length"
	"github.com/ledgerwatch/erigon-lib/compress"
	"github.com/ledgerwatch/erigon-lib/recsplit"
)

// Defines how to evaluate commitments
type CommitmentMode uint

const (
	CommitmentModeDisabled CommitmentMode = 0
	CommitmentModeDirect   CommitmentMode = 1
	CommitmentModeUpdate   CommitmentMode = 2
)

type ValueMerger func(prev, current []byte) (merged []byte, err error)

type DomainCommitted struct {
	*Domain
	mode         CommitmentMode
	trace        bool
	commTree     *btree.BTreeG[*CommitmentItem]
	keccak       hash.Hash
	patriciaTrie *commitment.HexPatriciaHashed
	keyReplaceFn ValueMerger // defines logic performed with stored values during files merge
	branchMerger *commitment.BranchMerger
}

func NewCommittedDomain(d *Domain, mode CommitmentMode) *DomainCommitted {
	return &DomainCommitted{
		Domain:       d,
		patriciaTrie: commitment.NewHexPatriciaHashed(length.Addr, nil, nil, nil),
		commTree:     btree.NewG[*CommitmentItem](32, commitmentItemLess),
		keccak:       sha3.NewLegacyKeccak256(),
		mode:         mode,
		branchMerger: commitment.NewHexBranchMerger(8192),
	}
}

func (d *DomainCommitted) SetKeyReplacer(vm ValueMerger) { d.keyReplaceFn = vm }

func (d *DomainCommitted) SetCommitmentMode(m CommitmentMode) { d.mode = m }

// TouchPlainKey marks plainKey as updated and applies different fn for different key types
// (different behaviour for Code, Account and Storage key modifications).
func (d *DomainCommitted) TouchPlainKey(key, val []byte, fn func(c *CommitmentItem, val []byte)) {
	if d.mode == CommitmentModeDisabled {
		return
	}
	c := &CommitmentItem{plainKey: common.Copy(key), hashedKey: d.hashAndNibblizeKey(key)}
	if d.mode > CommitmentModeDirect {
		fn(c, val)
	}
	d.commTree.ReplaceOrInsert(c)
}

func (d *DomainCommitted) TouchPlainKeyAccount(c *CommitmentItem, val []byte) {
	if len(val) == 0 {
		c.update.Flags = commitment.DELETE_UPDATE
		return
	}
	c.update.DecodeForStorage(val)
	c.update.Flags = commitment.BALANCE_UPDATE | commitment.NONCE_UPDATE
	item, found := d.commTree.Get(&CommitmentItem{hashedKey: c.hashedKey})
	if !found {
		return
	}
	if item.update.Flags&commitment.CODE_UPDATE != 0 {
		c.update.Flags |= commitment.CODE_UPDATE
		copy(c.update.CodeHashOrStorage[:], item.update.CodeHashOrStorage[:])
	}
}

func (d *DomainCommitted) TouchPlainKeyStorage(c *CommitmentItem, val []byte) {
	c.update.ValLength = len(val)
	if len(val) == 0 {
		c.update.Flags = commitment.DELETE_UPDATE
	} else {
		c.update.Flags = commitment.STORAGE_UPDATE
		copy(c.update.CodeHashOrStorage[:], val)
	}
}

func (d *DomainCommitted) TouchPlainKeyCode(c *CommitmentItem, val []byte) {
	c.update.Flags = commitment.CODE_UPDATE
	item, found := d.commTree.Get(c)
	if !found {
		d.keccak.Reset()
		d.keccak.Write(val)
		copy(c.update.CodeHashOrStorage[:], d.keccak.Sum(nil))
		return
	}
	if item.update.Flags&commitment.BALANCE_UPDATE != 0 {
		c.update.Flags |= commitment.BALANCE_UPDATE
		c.update.Balance.Set(&item.update.Balance)
	}
	if item.update.Flags&commitment.NONCE_UPDATE != 0 {
		c.update.Flags |= commitment.NONCE_UPDATE
		c.update.Nonce = item.update.Nonce
	}
	if item.update.Flags == commitment.DELETE_UPDATE && len(val) == 0 {
		c.update.Flags = commitment.DELETE_UPDATE
	} else {
		d.keccak.Reset()
		d.keccak.Write(val)
		copy(c.update.CodeHashOrStorage[:], d.keccak.Sum(nil))
	}
}

type CommitmentItem struct {
	plainKey  []byte
	hashedKey []byte
	update    commitment.Update
}

func commitmentItemLess(i, j *CommitmentItem) bool {
	return bytes.Compare(i.hashedKey, j.hashedKey) < 0
}

// Returns list of both plain and hashed keys. If .mode is CommitmentModeUpdate, updates also returned.
func (d *DomainCommitted) TouchedKeyList() ([][]byte, [][]byte, []commitment.Update) {
	plainKeys := make([][]byte, d.commTree.Len())
	hashedKeys := make([][]byte, d.commTree.Len())
	updates := make([]commitment.Update, d.commTree.Len())

	j := 0
	d.commTree.Ascend(func(item *CommitmentItem) bool {
		plainKeys[j] = item.plainKey
		hashedKeys[j] = item.hashedKey
		updates[j] = item.update
		j++
		return true
	})

	d.commTree.Clear(true)
	return plainKeys, hashedKeys, updates
}

// TODO(awskii): let trie define hashing function
func (d *DomainCommitted) hashAndNibblizeKey(key []byte) []byte {
	hashedKey := make([]byte, length.Hash)

	d.keccak.Reset()
	d.keccak.Write(key[:length.Addr])
	copy(hashedKey[:length.Hash], d.keccak.Sum(nil))

	if len(key[length.Addr:]) > 0 {
		hashedKey = append(hashedKey, make([]byte, length.Hash)...)
		d.keccak.Reset()
		d.keccak.Write(key[length.Addr:])
		copy(hashedKey[length.Hash:], d.keccak.Sum(nil))
	}

	nibblized := make([]byte, len(hashedKey)*2)
	for i, b := range hashedKey {
		nibblized[i*2] = (b >> 4) & 0xf
		nibblized[i*2+1] = b & 0xf
	}
	return nibblized
}

func (d *DomainCommitted) storeCommitmentState(blockNum, txNum uint64) error {
	state, err := d.patriciaTrie.EncodeCurrentState(nil)
	if err != nil {
		return err
	}
	cs := &commitmentState{txNum: txNum, trieState: state, blockNum: blockNum}
	encoded, err := cs.Encode()
	if err != nil {
		return err
	}

	var stepbuf [2]byte
	step := uint16(txNum / d.aggregationStep)
	binary.BigEndian.PutUint16(stepbuf[:], step)
	if err = d.Domain.Put(keyCommitmentState, stepbuf[:], encoded); err != nil {
		return err
	}
	return nil
}

// nolint
func (d *DomainCommitted) replaceKeyWithReference(fullKey, shortKey []byte, typeAS string, list ...*filesItem) bool {
	numBuf := [2]byte{}
	var found bool
	for _, item := range list {
		g := item.decompressor.MakeGetter()
		index := recsplit.NewIndexReader(item.index)

		offset := index.Lookup(fullKey)
		g.Reset(offset)
		if !g.HasNext() {
			continue
		}
		if keyMatch, _ := g.Match(fullKey); keyMatch {
			step := uint16(item.endTxNum / d.aggregationStep)
			binary.BigEndian.PutUint16(numBuf[:], step)

			shortKey = encodeU64(offset, numBuf[:])

			if d.trace {
				fmt.Printf("replacing %s [%x] => {%x} [step=%d, offset=%d, file=%s.%d-%d]\n", typeAS, fullKey, shortKey, step, offset, typeAS, item.startTxNum, item.endTxNum)
			}
			found = true
			break
		}
	}
	return found
}

func (d *DomainCommitted) lookupShortenedKey(shortKey, fullKey []byte, typAS string, list []*filesItem) bool {
	fileStep, offset := shortenedKey(shortKey)
	expected := uint64(fileStep) * d.aggregationStep
	var size uint64
	switch typAS {
	case "account":
		size = length.Addr
	case "storage":
		size = length.Addr + length.Hash
	default:
		return false
	}

	var found bool
	for _, item := range list {
		if item.startTxNum > expected || item.endTxNum < expected {
			continue
		}
		g := item.decompressor.MakeGetter()
		if uint64(g.Size()) <= offset+size {
			continue
		}
		g.Reset(offset)
		fullKey, _ = g.Next(fullKey[:0])
		if d.trace {
			fmt.Printf("offsetToKey %s [%x]=>{%x} step=%d offset=%d, file=%s.%d-%d.kv\n", typAS, fullKey, shortKey, fileStep, offset, typAS, item.startTxNum, item.endTxNum)
		}
		found = true
		break
	}
	return found
}

// commitmentValTransform parses the value of the commitment record to extract references
// to accounts and storage items, then looks them up in the new, merged files, and replaces them with
// the updated references
func (d *DomainCommitted) commitmentValTransform(files *SelectedStaticFiles, merged *MergedFiles, val commitment.BranchData) ([]byte, error) {
	if len(val) == 0 {
		return nil, nil
	}
	accountPlainKeys, storagePlainKeys, err := val.ExtractPlainKeys()
	if err != nil {
		return nil, err
	}

	transAccountPks := make([][]byte, 0, len(accountPlainKeys))
	var apkBuf, spkBuf []byte
	for _, accountPlainKey := range accountPlainKeys {
		if len(accountPlainKey) == length.Addr {
			// Non-optimised key originating from a database record
			apkBuf = append(apkBuf[:0], accountPlainKey...)
		} else {
			f := d.lookupShortenedKey(accountPlainKey, apkBuf, "account", files.accounts)
			if !f {
				fmt.Printf("lost key %x\n", accountPlainKeys)
			}
		}
		d.replaceKeyWithReference(apkBuf, accountPlainKey, "account", merged.accounts)
		transAccountPks = append(transAccountPks, accountPlainKey)
	}

	transStoragePks := make([][]byte, 0, len(storagePlainKeys))
	for _, storagePlainKey := range storagePlainKeys {
		if len(storagePlainKey) == length.Addr+length.Hash {
			// Non-optimised key originating from a database record
			spkBuf = append(spkBuf[:0], storagePlainKey...)
		} else {
			// Optimised key referencing a state file record (file number and offset within the file)
			f := d.lookupShortenedKey(storagePlainKey, spkBuf, "storage", files.storage)
			if !f {
				fmt.Printf("lost skey %x\n", storagePlainKey)
			}
		}

		d.replaceKeyWithReference(spkBuf, storagePlainKey, "storage", merged.storage)
		transStoragePks = append(transStoragePks, storagePlainKey)
	}

	transValBuf, err := val.ReplacePlainKeys(transAccountPks, transStoragePks, nil)
	if err != nil {
		return nil, err
	}
	return transValBuf, nil
}

func (d *DomainCommitted) mergeFiles(ctx context.Context, oldFiles SelectedStaticFiles, mergedFiles MergedFiles, r DomainRanges, workers int) (valuesIn, indexIn, historyIn *filesItem, err error) {
	if !r.any() {
		return
	}

	valuesFiles := oldFiles.commitment
	indexFiles := oldFiles.commitmentIdx
	historyFiles := oldFiles.commitmentHist

	var comp *compress.Compressor
	var closeItem bool = true
	defer func() {
		if closeItem {
			if comp != nil {
				comp.Close()
			}
			if indexIn != nil {
				if indexIn.decompressor != nil {
					indexIn.decompressor.Close()
				}
				if indexIn.index != nil {
					indexIn.index.Close()
				}
			}
			if historyIn != nil {
				if historyIn.decompressor != nil {
					historyIn.decompressor.Close()
				}
				if historyIn.index != nil {
					historyIn.index.Close()
				}
			}
			if valuesIn != nil {
				if valuesIn.decompressor != nil {
					valuesIn.decompressor.Close()
				}
				if valuesIn.index != nil {
					valuesIn.index.Close()
				}
			}
		}
	}()
	if indexIn, historyIn, err = d.History.mergeFiles(ctx, indexFiles, historyFiles,
		HistoryRanges{
			historyStartTxNum: r.historyStartTxNum,
			historyEndTxNum:   r.historyEndTxNum,
			history:           r.history,
			indexStartTxNum:   r.indexStartTxNum,
			indexEndTxNum:     r.indexEndTxNum,
			index:             r.index}, workers); err != nil {
		return nil, nil, nil, err
	}
	if r.values {
		datPath := filepath.Join(d.dir, fmt.Sprintf("%s.%d-%d.kv", d.filenameBase, r.valuesStartTxNum/d.aggregationStep, r.valuesEndTxNum/d.aggregationStep))
		if comp, err = compress.NewCompressor(context.Background(), "merge", datPath, d.dir, compress.MinPatternScore, workers, log.LvlDebug); err != nil {
			return nil, nil, nil, fmt.Errorf("merge %s history compressor: %w", d.filenameBase, err)
		}
		var cp CursorHeap
		heap.Init(&cp)
		for _, item := range valuesFiles {
			g := item.decompressor.MakeGetter()
			g.Reset(0)
			if g.HasNext() {
				key, _ := g.NextUncompressed()
				var val []byte
				if d.compressVals {
					val, _ = g.Next(nil)
				} else {
					val, _ = g.NextUncompressed()
				}
				if d.trace {
					fmt.Printf("merge: read value '%x'\n", key)
				}
				heap.Push(&cp, &CursorItem{
					t:        FILE_CURSOR,
					dg:       g,
					key:      key,
					val:      val,
					endTxNum: item.endTxNum,
					reverse:  true,
				})
			}
		}
		keyCount := 0
		// In the loop below, the pair `keyBuf=>valBuf` is always 1 item behind `lastKey=>lastVal`.
		// `lastKey` and `lastVal` are taken from the top of the multi-way merge (assisted by the CursorHeap cp), but not processed right away
		// instead, the pair from the previous iteration is processed first - `keyBuf=>valBuf`. After that, `keyBuf` and `valBuf` are assigned
		// to `lastKey` and `lastVal` correspondingly, and the next step of multi-way merge happens. Therefore, after the multi-way merge loop
		// (when CursorHeap cp is empty), there is a need to process the last pair `keyBuf=>valBuf`, because it was one step behind
		var keyBuf, valBuf []byte
		for cp.Len() > 0 {
			lastKey := common.Copy(cp[0].key)
			lastVal := common.Copy(cp[0].val)
			// Advance all the items that have this key (including the top)
			for cp.Len() > 0 && bytes.Equal(cp[0].key, lastKey) {
				ci1 := cp[0]
				if ci1.dg.HasNext() {
					ci1.key, _ = ci1.dg.NextUncompressed()
					if d.compressVals {
						ci1.val, _ = ci1.dg.Next(ci1.val[:0])
					} else {
						ci1.val, _ = ci1.dg.NextUncompressed()
					}
					heap.Fix(&cp, 0)
				} else {
					heap.Pop(&cp)
				}
			}
			var skip bool
			if d.prefixLen > 0 {
				skip = r.valuesStartTxNum == 0 && len(lastVal) == 0 && len(lastKey) != d.prefixLen
			} else {
				// For the rest of types, empty value means deletion
				skip = r.valuesStartTxNum == 0 && len(lastVal) == 0
			}
			if !skip {
				if keyBuf != nil && (d.prefixLen == 0 || len(keyBuf) != d.prefixLen || bytes.HasPrefix(lastKey, keyBuf)) {
					if err = comp.AddUncompressedWord(keyBuf); err != nil {
						return nil, nil, nil, err
					}
					keyCount++ // Only counting keys, not values

					if d.trace {
						fmt.Printf("merge: multi-way key %x, total keys %d\n", keyBuf, keyCount)
					}

					valBuf, err = d.commitmentValTransform(&oldFiles, &mergedFiles, valBuf)
					if err != nil {
						return nil, nil, nil, fmt.Errorf("merge: valTransform [%x] %w", valBuf, err)
					}
					if d.compressVals {
						if err = comp.AddWord(valBuf); err != nil {
							return nil, nil, nil, err
						}
					} else {
						if err = comp.AddUncompressedWord(valBuf); err != nil {
							return nil, nil, nil, err
						}
					}
				}
				keyBuf = append(keyBuf[:0], lastKey...)
				valBuf = append(valBuf[:0], lastVal...)
			}
		}
		if keyBuf != nil {
			if err = comp.AddUncompressedWord(keyBuf); err != nil {
				return nil, nil, nil, err
			}
			keyCount++ // Only counting keys, not values
			//fmt.Printf("last heap key %x\n", keyBuf)
			valBuf, err = d.commitmentValTransform(&oldFiles, &mergedFiles, valBuf)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("merge: 2valTransform [%x] %w", valBuf, err)
			}
			if d.compressVals {
				if err = comp.AddWord(valBuf); err != nil {
					return nil, nil, nil, err
				}
			} else {
				if err = comp.AddUncompressedWord(valBuf); err != nil {
					return nil, nil, nil, err
				}
			}
		}
		if err = comp.Compress(); err != nil {
			return nil, nil, nil, err
		}
		comp.Close()
		comp = nil
		idxPath := filepath.Join(d.dir, fmt.Sprintf("%s.%d-%d.kvi", d.filenameBase, r.valuesStartTxNum/d.aggregationStep, r.valuesEndTxNum/d.aggregationStep))
		valuesIn = &filesItem{startTxNum: r.valuesStartTxNum, endTxNum: r.valuesEndTxNum}
		if valuesIn.decompressor, err = compress.NewDecompressor(datPath); err != nil {
			return nil, nil, nil, fmt.Errorf("merge %s decompressor [%d-%d]: %w", d.filenameBase, r.valuesStartTxNum, r.valuesEndTxNum, err)
		}
		if valuesIn.index, err = buildIndex(ctx, valuesIn.decompressor, idxPath, d.dir, keyCount, false /* values */); err != nil {
			return nil, nil, nil, fmt.Errorf("merge %s buildIndex [%d-%d]: %w", d.filenameBase, r.valuesStartTxNum, r.valuesEndTxNum, err)
		}
	}
	closeItem = false
	d.stats.MergesCount++
	d.mergesCount++
	return
}

// Evaluates commitment for processed state. Commit=true - store trie state after evaluation
func (d *DomainCommitted) ComputeCommitment(trace bool) (rootHash []byte, branchNodeUpdates map[string]commitment.BranchData, err error) {
	touchedKeys, hashedKeys, updates := d.TouchedKeyList()
	if len(touchedKeys) == 0 {
		rootHash, err = d.patriciaTrie.RootHash()
		return rootHash, nil, err
	}

	// data accessing functions should be set once before
	d.patriciaTrie.Reset()
	d.patriciaTrie.SetTrace(trace)

	switch d.mode {
	case CommitmentModeDirect:
		rootHash, branchNodeUpdates, err = d.patriciaTrie.ReviewKeys(touchedKeys, hashedKeys)
		if err != nil {
			return nil, nil, err
		}
	case CommitmentModeUpdate:
		rootHash, branchNodeUpdates, err = d.patriciaTrie.ProcessUpdates(touchedKeys, hashedKeys, updates)
		if err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("invalid commitment mode: %d", d.mode)
	}
	return rootHash, branchNodeUpdates, err
}

var keyCommitmentState = []byte("state")

// SeekCommitment searches for last encoded state from DomainCommitted
// and if state found, sets it up to current domain
func (d *DomainCommitted) SeekCommitment(aggStep, sinceTx uint64) (uint64, error) {
	var (
		latestState []byte
		stepbuf     [2]byte
		step        uint16 = uint16(sinceTx/aggStep) - 1
		latestTxNum uint64 = sinceTx - 1
	)
	d.SetTxNum(latestTxNum)
	ctx := d.MakeContext()

	for {
		binary.BigEndian.PutUint16(stepbuf[:], step)

		s, err := ctx.Get(keyCommitmentState, stepbuf[:], d.tx)
		if err != nil {
			return 0, err
		}
		if len(s) < 8 {
			break
		}
		v := binary.BigEndian.Uint64(s)
		if v == latestTxNum && len(latestState) != 0 {
			break
		}
		latestTxNum, latestState = v, s
		lookupTxN := latestTxNum + aggStep // - 1
		step = uint16(latestTxNum/aggStep) + 1
		d.SetTxNum(lookupTxN)
	}

	var latest commitmentState
	if err := latest.Decode(latestState); err != nil {
		return 0, nil
	}

	if err := d.patriciaTrie.SetState(latest.trieState); err != nil {
		return 0, err
	}
	return latest.txNum, nil
}

type commitmentState struct {
	txNum     uint64
	blockNum  uint64
	trieState []byte
}

func (cs *commitmentState) Decode(buf []byte) error {
	if len(buf) < 10 {
		return fmt.Errorf("ivalid commitment state buffer size")
	}
	pos := 0
	cs.txNum = binary.BigEndian.Uint64(buf[pos : pos+8])
	pos += 8
	cs.blockNum = binary.BigEndian.Uint64(buf[pos : pos+8])
	pos += 8
	cs.trieState = make([]byte, binary.BigEndian.Uint16(buf[pos:pos+2]))
	pos += 2
	if len(cs.trieState) == 0 && len(buf) == 10 {
		return nil
	}
	copy(cs.trieState, buf[pos:pos+len(cs.trieState)])
	return nil
}

func (cs *commitmentState) Encode() ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	var v [18]byte
	binary.BigEndian.PutUint64(v[:], cs.txNum)
	binary.BigEndian.PutUint64(v[8:16], cs.blockNum)
	binary.BigEndian.PutUint16(v[16:18], uint16(len(cs.trieState)))
	if _, err := buf.Write(v[:]); err != nil {
		return nil, err
	}
	if _, err := buf.Write(cs.trieState); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeU64(from []byte) uint64 {
	var i uint64
	for _, b := range from {
		i = (i << 8) | uint64(b)
	}
	return i
}

func encodeU64(i uint64, to []byte) []byte {
	// writes i to b in big endian byte order, using the least number of bytes needed to represent i.
	switch {
	case i < (1 << 8):
		return append(to, byte(i))
	case i < (1 << 16):
		return append(to, byte(i>>8), byte(i))
	case i < (1 << 24):
		return append(to, byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 32):
		return append(to, byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 40):
		return append(to, byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 48):
		return append(to, byte(i>>40), byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	case i < (1 << 56):
		return append(to, byte(i>>48), byte(i>>40), byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	default:
		return append(to, byte(i>>56), byte(i>>48), byte(i>>40), byte(i>>32), byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
	}
}

// Optimised key referencing a state file record (file number and offset within the file)
func shortenedKey(apk []byte) (step uint16, offset uint64) {
	step = binary.BigEndian.Uint16(apk[:2])
	return step, decodeU64(apk[1:])
}
