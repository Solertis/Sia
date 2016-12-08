package renter

import (
	"io"
	"os"
	"time"

	"github.com/NebulousLabs/Sia/types"
)

type (
	// chunkGaps points to all of the missing pieces in a chunk, as well as all
	// of the hosts that are missing. The 'numGaps' value is the minimum of
	// len(contracts) and len(pieces).
	chunkGaps struct {
		contracts []types.FileContractID
		pieces    []uint64
	}

	// chunkID can be used to uniquely identify a chunk within the repair
	// matrix.
	chunkID struct {
		chunkIndex uint64
		filename   string
	}
)

// addFileToRepairMatrix will take a file and add each of the incomplete chunks
// to the repair matrix.
func addFileToRepairMatrix(file *file, availableWorkers map[types.FileContractID]struct{}, repairMatrix map[chunkID]*chunkGaps, gapCounts map[int]int) {
	// Flatten availableWorkers into a list of contracts.
	contracts := make([]types.FileContractID, 0)
	for contract := range availableWorkers {
		contracts = append(contracts, contract)
	}

	// For each chunk, create a map from the chunk to the pieces that it
	// has, and to the contracts that have that chunk.
	chunkCount := file.numChunks()
	completedPieces := make(map[uint64][]uint64)
	utilizedContracts := make(map[uint64][]types.FileContractID)
	for i := uint64(0); i < chunkCount; i++ {
		completedPieces[i] = make([]uint64, 0)
		utilizedContracts[i] = make([]types.FileContractID, 0)
	}

	// Iterate through each contract and figure out what's available.
	for _, contract := range file.contracts {
		// Grab the contract from the contractor and see if the corresponding
		// host is online.
		for _, piece := range contract.Pieces {
			utilizedContracts[piece.Chunk] = append(utilizedContracts[piece.Chunk], contract.ID)

			// Only mark the piece as available if the piece can be recovered.
			//
			// TODO: Add an 'unavailable' flag to the piece that gets set to
			// true if the host loses the piece, and only add the piece to the
			// 'completedPieces' set if !unavailable.
			pieceMap[piece.Chunk] = append(pieceMap[piece.Chunk], piece.Piece)
		}
	}

	// Iterate through each chunk and, if there are gaps, add the inverse
	// to the repair matrix.
	for i := uint64(0); i < chunkCount; i++ {
		if len(pieceMap[i]) < file.erasureCode.NumPieces() {
			// Find the gaps in the pieces and contracts.
			potentialPieceGaps := make([]bool, file.erasureCode.NumPieces())
			potentialContractGaps := make(map[types.FileContractID]struct{})
			for _, contract := range contracts {
				potentialContractGaps[contract] = struct{}{}
			}

			// Delete every available piece from the potential piece gaps,
			// and every utilized contract form the potential contract
			// gaps.
			for _, piece := range pieceMap[i] {
				potentialPieceGaps[piece] = true
			}
			for _, fcid := range contractMap[i] {
				delete(potentialContractGaps, fcid)
			}

			// Merge the gaps into a slice.
			var gaps chunkGaps
			for j, piece := range potentialPieceGaps {
				if !piece {
					gaps.pieces = append(gaps.pieces, uint64(j))
				}
			}
			for fcid := range potentialContractGaps {
				gaps.contracts = append(gaps.contracts, fcid)
			}

			// Record the number of gaps that this chunk has, which makes
			// blocking-related decisions easier.
			gapCounts[gaps.numGaps()]++

			// Add the chunk to the repair matrix.
			cid := chunkID{i, file.name}
			repairMatrix[cid] = &gaps
		}
	}
}

// numGaps returns the number of gaps that a chunk has.
func (cg *chunkGaps) numGaps() int {
	if len(cg.contracts) <= len(cg.pieces) {
		return len(cg.contracts)
	}
	return len(cg.pieces)
}

func (r *Renter) createRepairMatrix(availableWorkers map[types.FileContractID]struct{}) (map[chunkID]*chunkGaps, map[int]int) {
	repairMatrix := make(map[chunkID]*chunkGaps)
	gapCounts := make(map[int]int)

	// Add all of the files to the repair matrix.
	for _, file := range r.files {
		_, exists := r.tracking[file.name]
		if !exists {
			continue
		}
		file.mu.Lock()
		addFileToRepairMatrix(file, availableWorkers, repairMatrix, gapCounts)
		file.mu.Unlock()
	}
	return repairMatrix, gapCounts
}

func (r *Renter) managedRepairIteration() {
	// Create the initial set of workers that are used to perform
	// uploading.
	availableWorkers := make(map[types.FileContractID]struct{})
	id := r.mu.RLock()
	for id, worker := range r.workerPool {
		// Ignore workers that have had an upload failure in the past two
		// hours.
		if worker.recentUploadFailure.Add(time.Hour).Before(time.Now()) {
			availableWorkers[id] = struct{}{}
		}
	}
	r.mu.RUnlock(id)

	// Create the repair matrix. The repair matrix is a set of chunks,
	// linked from chunk id to the set of hosts that do not have that
	// chunk.
	id = r.mu.Lock()
	repairMatrix, gapCounts := r.createRepairMatrix(availableWorkers)
	r.mu.Unlock(id)

	// Determine the maximum number of gaps of any chunk in the repair matrix.
	maxGaps := 0
	for i, gaps := range gapCounts {
		if i > maxGaps && gaps > 0 {
			maxGaps = i
		}
	}
	if maxGaps == 0 {
		// There is no work to do. Sleep for 15 minutes, or until there has
		// been a new upload. Then iterate to make a new repair matrix and
		// check again.
		select {
		case <-r.tg.StopChan():
			return
		case <-time.After(time.Minute * 15):
			return
		case <-r.newFiles:
			return
		}
	}

	// Set up a loop that first waits for enough workers to become
	// available, and then iterates through the repair matrix to find a
	// chunk to repair. The loop will create a chunk if as few as 4 pieces
	// can be handed off to workers simultaneously.
	startTime := time.Now()
	activeWorkers := make(map[types.FileContractID]struct{})
	for k, v := range availableWorkers {
		activeWorkers[k] = v
	}
	var retiredWorkers []types.FileContractID
	resultChan := make(chan finishedUpload)
	for {
		// Break if tg.Stop() has been called, to facilitate quick shutdown.
		select {
		case <-r.tg.StopChan():
			break
		default:
			// Stop is not called, continue with the iteration.
		}

		// Determine the maximum number of gaps that any chunk has.
		maxGaps := 0
		for i, gaps := range gapCounts {
			if i > maxGaps && gaps > 0 {
				maxGaps = i
			}
		}
		if maxGaps == 0 {
			// None of the chunks have any more opportunity to upload.
			break
		}

		// Iterate through the chunks until a candidate chunk is found.
		for chunkID, chunkGaps := range repairMatrix {
			// Figure out how many pieces in this chunk could be repaired
			// by the current availableWorkers.
			var usefulWorkers []types.FileContractID
			for worker := range availableWorkers {
				for _, contract := range chunkGaps.contracts {
					if worker == contract {
						usefulWorkers = append(usefulWorkers, worker)
					}
				}
			}

			if maxGaps >= 4 && len(usefulWorkers) < 4 {
				// These workers in particular are not useful for this
				// chunk - need a different or broader set of workers.
				// Update the gapCount for this chunk - retired workers may
				// have altered the number.

				// Remove the contract ids of any workers that have
				// retired.
				for _, retire := range retiredWorkers {
					for i := range chunkGaps.contracts {
						if chunkGaps.contracts[i] == retire {
							chunkGaps.contracts = append(chunkGaps.contracts[:i], chunkGaps.contracts[i+1:]...)
							break
						}
					}
				}
				// Update the gap counts if they have been affected in any
				// way.
				if len(chunkGaps.contracts) < len(chunkGaps.pieces) && len(chunkGaps.contracts) < chunkGaps.numGaps() {
					oldNumGaps := chunkGaps.numGaps()
					chunkGaps.numGaps = len(chunkGaps.contracts)
					gapCounts[oldNumGaps]--
					gapCounts[chunkGaps.numGaps()]++
				}
				continue
			}

			// Parse the filename and chunk index from the repair
			// matrix key.
			chunkIndex := chunkID.chunkIndex
			filename := chunkID.filename
			id := r.mu.RLock()
			file, exists := r.files[filename]
			r.mu.RUnlock(id)
			if !exists {
				// TODO: Should pull this chunk out of the repair
				// matrix. The other errors in this block should do the
				// same.
				continue
			}

			// Grab the chunk and code it into its separate pieces.
			id = r.mu.RLock()
			meta, exists := r.tracking[filename]
			r.mu.RUnlock(id)
			if !exists {
				continue
			}
			fHandle, err := os.Open(meta.RepairPath)
			if err != nil {
				// TODO: Perform a download-and-repair. Though, this
				// may block other uploads that are in progress. Not
				// sure how to do this cleanly in the background?
				//
				// TODO: Manage err
				continue
			}
			defer fHandle.Close()
			chunk := make([]byte, file.chunkSize())
			_, err = fHandle.ReadAt(chunk, int64(chunkIndex*file.chunkSize()))
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				// TODO: Manage this error.
				continue
			}
			pieces, err := file.erasureCode.Encode(chunk)
			if err != nil {
				// TODO: Manage this error.
				continue
			}
			// encrypt pieces
			for i := range pieces {
				key := deriveKey(file.masterKey, chunkIndex, uint64(i))
				pieces[i], err = key.EncryptBytes(pieces[i])
				if err != nil {
					// TODO: Manage this error.
					continue
				}
			}

			// Give each piece to a worker, updating the chunkGaps and
			// availableWorkers along the way.
			var i int
			for i = 0; i < len(usefulWorkers) && i < len(chunkGaps.pieces); i++ {
				uw := uploadWork{
					chunkIndex: chunkIndex,
					data:       pieces[chunkGaps.pieces[i]],
					file:       file,
					pieceIndex: chunkGaps.pieces[i],

					resultChan: resultChan,
				}
				worker := r.workerPool[usefulWorkers[i]]
				select {
				case worker.uploadChan <- uw:
				case <-r.tg.StopChan():
					return
				}

				delete(availableWorkers, usefulWorkers[i])
				for j := 0; j < len(chunkGaps.contracts); j++ {
					if chunkGaps.contracts[j] == usefulWorkers[i] {
						chunkGaps.contracts = append(chunkGaps.contracts[:j], chunkGaps.contracts[j+1:]...)
						break
					}
				}
			}
			chunkGaps.pieces = chunkGaps.pieces[i:]

			// Update the number of gaps.
			oldNumGaps := chunkGaps.numGaps
			if len(chunkGaps.contracts) < len(chunkGaps.pieces) {
				chunkGaps.numGaps = len(chunkGaps.contracts)
			} else {
				chunkGaps.numGaps = len(chunkGaps.pieces)
			}
			gapCounts[oldNumGaps] = gapCounts[oldNumGaps] - 1
			gapCounts[chunkGaps.numGaps] = gapCounts[chunkGaps.numGaps] + 1
			break
		}

		// Determine the number of workers we need in 'available'.
		exclude := maxGaps - 4
		if exclude < 0 {
			exclude = 0
		}
		need := len(activeWorkers) - exclude
		if need <= len(availableWorkers) {
			need = len(availableWorkers) + 1
		}
		if need > len(activeWorkers) {
			need = len(activeWorkers)
		}
		newMatrix := false
		if time.Since(startTime) > time.Hour {
			newMatrix = true
			need = len(activeWorkers)
		}

		// Wait until 'need' workers are available.
		for len(availableWorkers) < need {
			var finishedUpload finishedUpload
			select {
			case finishedUpload = <-resultChan:
			case <-r.tg.StopChan():
				return
			}

			if finishedUpload.err != nil {
				r.log.Debugln("Error while performing upload to", finishedUpload.workerID, "::", finishedUpload.err)
				id := r.mu.Lock()
				worker, exists := r.workerPool[finishedUpload.workerID]
				if exists {
					worker.recentUploadFailure = time.Now()
					retiredWorkers = append(retiredWorkers, finishedUpload.workerID)
					delete(activeWorkers, finishedUpload.workerID)
					need--
				}
				r.mu.Unlock(id)
				continue
			}

			// Mark that the worker is available again.
			availableWorkers[finishedUpload.workerID] = struct{}{}
		}

		// Grab a new repair matrix if we've been using this repair matrix
		// for more than an hour.
		if newMatrix {
			break
		}

		// Receive all of the new files and add them to the repair matrix
		// before continuing.
		var done bool
		for !done {
			select {
			case file := <-r.newFiles:
				addFileToRepairMatrix(file, activeWorkers, repairMatrix, gapCounts)
			default:
				done = true
			}
		}
	}
}

// threadedRepairLoop improves the health of files tracked by the renter by
// reuploading their missing pieces. Multiple repair attempts may be necessary
// before the file reaches full redundancy.
func (r *Renter) threadedRepairLoop() {
	for {
		if r.tg.Add() != nil {
			return
		}
		r.managedRepairIteration()
		r.tg.Done()
	}
}
