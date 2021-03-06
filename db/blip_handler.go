package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/couchbase/go-blip"
	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/channels"
)

// kHandlersByProfile defines the routes for each message profile (verb) of an incoming request to the function that handles it.
var kHandlersByProfile = map[string]blipHandlerFunc{
	MessageGetCheckpoint:  (*blipHandler).handleGetCheckpoint,
	MessageSetCheckpoint:  (*blipHandler).handleSetCheckpoint,
	MessageSubChanges:     userBlipHandler((*blipHandler).handleSubChanges),
	MessageChanges:        userBlipHandler((*blipHandler).handleChanges),
	MessageRev:            userBlipHandler((*blipHandler).handleRev),
	MessageNoRev:          (*blipHandler).handleNoRev,
	MessageGetAttachment:  userBlipHandler((*blipHandler).handleGetAttachment),
	MessageProposeChanges: (*blipHandler).handleProposeChanges,
}

type blipHandler struct {
	*BlipSyncContext
	db           *Database // Handler-specific copy of the BlipSyncContext's blipContextDb
	serialNumber uint64    // This blip handler's serial number to differentiate logs w/ other handlers
}

type blipHandlerFunc func(*blipHandler, *blip.Message) error

// userBlipHandler wraps another blip handler with code that reloads the user object when the user
// or the user's roles have changed, to make sure that the replication has the latest channel access grants.
// Uses a userChangeWaiter to detect changes to the user or roles.  Note that in the case of a pushed document
// triggering a user access change, this happens at write time (via MarkPrincipalsChanged), and doesn't
// depend on the userChangeWaiter.
func userBlipHandler(next blipHandlerFunc) blipHandlerFunc {
	return func(bh *blipHandler, bm *blip.Message) error {

		// Reload user if it has changed
		if err := bh.refreshUser(); err != nil {
			return err
		}
		// Call down to the underlying handler and return it's value
		return next(bh, bm)
	}
}

func (bh *blipHandler) refreshUser() error {

	bc := bh.BlipSyncContext
	if bc.userName != "" {
		// Check whether user needs to be refreshed
		bc.dbUserLock.Lock()
		userChanged := bc.userChangeWaiter.RefreshUserCount()

		// If changed, refresh the user and db while holding the lock
		if userChanged {
			// Refresh the BlipSyncContext database
			newUser, err := bc.blipContextDb.Authenticator().GetUser(bc.userName)
			if err != nil {
				bc.dbUserLock.Unlock()
				return err
			}
			bc.userChangeWaiter.RefreshUserKeys(newUser)
			bc.blipContextDb.SetUser(newUser)

			// refresh the handler's database with the new BlipSyncContext database
			bh.db = bh._copyContextDatabase()
		}
		bc.dbUserLock.Unlock()
	}
	return nil
}

//////// CHECKPOINTS

// Received a "getCheckpoint" request
func (bh *blipHandler) handleGetCheckpoint(rq *blip.Message) error {

	client := rq.Properties[BlipClient]
	bh.logEndpointEntry(rq.Profile(), fmt.Sprintf("Client:%s", client))

	docID := fmt.Sprintf("checkpoint/%s", client)
	response := rq.Response()
	if response == nil {
		return nil
	}

	value, err := bh.db.GetSpecial("local", docID)
	if err != nil {
		return err
	}
	if value == nil {
		return base.HTTPErrorf(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}
	response.Properties[GetCheckpointResponseRev] = value[BodyRev].(string)
	delete(value, BodyRev)
	delete(value, BodyId)
	// TODO: Marshaling here when we could use raw bytes all the way from the bucket
	_ = response.SetJSONBody(value)
	return nil
}

// Received a "setCheckpoint" request
func (bh *blipHandler) handleSetCheckpoint(rq *blip.Message) error {

	checkpointMessage := SetCheckpointMessage{rq}
	bh.logEndpointEntry(rq.Profile(), checkpointMessage.String())

	docID := fmt.Sprintf("checkpoint/%s", checkpointMessage.client())

	var checkpoint Body
	if err := checkpointMessage.ReadJSONBody(&checkpoint); err != nil {
		return err
	}
	if revID := checkpointMessage.rev(); revID != "" {
		checkpoint[BodyRev] = revID
	}
	revID, err := bh.db.PutSpecial("local", docID, checkpoint)
	if err != nil {
		return err
	}

	checkpointResponse := SetCheckpointResponse{checkpointMessage.Response()}
	checkpointResponse.setRev(revID)

	return nil
}

//////// CHANGES

// Received a "subChanges" subscription request
func (bh *blipHandler) handleSubChanges(rq *blip.Message) error {

	bh.lock.Lock()
	defer bh.lock.Unlock()

	bh.gotSubChanges = true

	logCtx := bh.BlipSyncContext.blipContextDb.Ctx
	subChangesParams, err := NewSubChangesParams(logCtx, rq, bh.db.CreateZeroSinceValue(), bh.db.ParseSequenceID)
	if err != nil {
		return base.HTTPErrorf(http.StatusBadRequest, "Invalid subChanges parameters")
	}

	// Ensure that only _one_ subChanges subscription can be open on this blip connection at any given time.  SG #3222.
	if !bh.activeSubChanges.CompareAndSwap(false, true) {
		return fmt.Errorf("blipHandler already has an outstanding continous subChanges.  Cannot open another one.")
	}

	if len(subChangesParams.docIDs()) > 0 && subChangesParams.continuous() {
		return base.HTTPErrorf(http.StatusBadRequest, "DocIDs filter not supported for continuous subChanges")
	}

	bh.logEndpointEntry(rq.Profile(), subChangesParams.String())

	// TODO: Do we need to store the changes-specific parameters on the blip sync context?  Seems like they only need to be passed in to sendChanges
	bh.batchSize = subChangesParams.batchSize()
	bh.continuous = subChangesParams.continuous()
	bh.activeOnly = subChangesParams.activeOnly()

	if filter := subChangesParams.filter(); filter == "sync_gateway/bychannel" {
		var err error

		bh.channels, err = subChangesParams.channelsExpandedSet()
		if err != nil {
			return base.HTTPErrorf(http.StatusBadRequest, "%s", err)
		} else if len(bh.channels) == 0 {
			return base.HTTPErrorf(http.StatusBadRequest, "Empty channel list")

		}
	} else if filter != "" {
		return base.HTTPErrorf(http.StatusBadRequest, "Unknown filter; try sync_gateway/bychannel")
	}

	// Start asynchronous changes goroutine
	go func() {
		// Pull replication stats by type - Active stats decremented in Close()
		if bh.continuous {
			bh.dbStats.StatsCblReplicationPull().Add(base.StatKeyPullReplicationsActiveContinuous, 1)
			bh.dbStats.StatsCblReplicationPull().Add(base.StatKeyPullReplicationsTotalContinuous, 1)
		} else {
			bh.dbStats.StatsCblReplicationPull().Add(base.StatKeyPullReplicationsActiveOneShot, 1)
			bh.dbStats.StatsCblReplicationPull().Add(base.StatKeyPullReplicationsTotalOneShot, 1)
		}

		defer func() {
			bh.activeSubChanges.Set(false)
		}()
		// sendChanges runs until blip context closes, or fails due to error
		startTime := time.Now()
		bh.sendChanges(rq.Sender, subChangesParams)
		base.DebugfCtx(bh.blipContextDb.Ctx, base.KeySyncMsg, "#%d: Type:%s   --> Time:%v", bh.serialNumber, rq.Profile(), time.Since(startTime))
	}()

	return nil
}

// Sends all changes since the given sequence
func (bh *blipHandler) sendChanges(sender *blip.Sender, params *SubChangesParams) {
	defer func() {
		if panicked := recover(); panicked != nil {
			base.Warnf("[%s] PANIC sending changes: %v\n%s", bh.blipContext.ID, panicked, debug.Stack())
		}
	}()

	base.InfofCtx(bh.blipContextDb.Ctx, base.KeySync, "Sending changes since %v", params.Since())

	options := ChangesOptions{
		Since:        params.Since(),
		Conflicts:    false, // CBL 2.0/BLIP don't support branched rev trees (LiteCore #437)
		Continuous:   bh.continuous,
		ActiveOnly:   bh.activeOnly,
		Terminator:   bh.BlipSyncContext.terminator,
		Ctx:          bh.db.Ctx,
		ClientIsCBL2: true,
	}

	channelSet := bh.channels
	if channelSet == nil {
		channelSet = base.SetOf(channels.AllChannelWildcard)
	}

	caughtUp := false
	pendingChanges := make([][]interface{}, 0, bh.batchSize)
	sendPendingChangesAt := func(minChanges int) error {
		if len(pendingChanges) >= minChanges {
			if err := bh.sendBatchOfChanges(sender, pendingChanges); err != nil {
				return err
			}
			pendingChanges = make([][]interface{}, 0, bh.batchSize)
		}
		return nil
	}

	// Create a distinct database instance for changes, to avoid races between reloadUser invocation in changes.go
	// and BlipSyncContext user access.
	changesDb := bh.copyContextDatabase()
	_, forceClose := generateBlipSyncChanges(changesDb, channelSet, options, params.docIDs(), func(changes []*ChangeEntry) error {
		base.DebugfCtx(bh.blipContextDb.Ctx, base.KeySync, "    Sending %d changes", len(changes))
		for _, change := range changes {

			if !strings.HasPrefix(change.ID, "_") {
				for _, item := range change.Changes {
					changeRow := []interface{}{change.Seq, change.ID, item["rev"], change.Deleted}
					if !change.Deleted {
						changeRow = changeRow[0:3]
					}
					pendingChanges = append(pendingChanges, changeRow)
					if err := sendPendingChangesAt(bh.batchSize); err != nil {
						return err
					}
				}
			}
		}
		if caughtUp || len(changes) == 0 {
			if err := sendPendingChangesAt(1); err != nil {
				return err
			}
			if !caughtUp {
				caughtUp = true
				// Signal to client that it's caught up
				if err := bh.sendBatchOfChanges(sender, nil); err != nil {
					return err
				}
			}
		}
		return nil
	})

	// On forceClose, send notify to trigger immediate exit from change waiter
	if forceClose && bh.db.User() != nil {
		bh.db.DatabaseContext.NotifyTerminatedChanges(bh.db.User().Name())
	}

}

func (bh *blipHandler) sendBatchOfChanges(sender *blip.Sender, changeArray [][]interface{}) error {
	outrq := blip.NewRequest()
	outrq.SetProfile("changes")
	err := outrq.SetJSONBody(changeArray)
	if err != nil {
		base.InfofCtx(bh.blipContextDb.Ctx, base.KeyAll, "Error setting changes: %v", err)
	}

	if len(changeArray) > 0 {
		// Check for user updates before creating the db copy for handleChangesResponse
		if err := bh.refreshUser(); err != nil {
			return err
		}
		handleChangesResponseDb := bh.copyContextDatabase()

		sendTime := time.Now()
		if !bh.sendBLIPMessage(sender, outrq) {
			return ErrClosedBLIPSender
		}

		// Spawn a goroutine to await the client's response:
		go func(bh *blipHandler, sender *blip.Sender, response *blip.Message, changeArray [][]interface{}, sendTime time.Time, database *Database) {
			if err := bh.handleChangesResponse(sender, response, changeArray, sendTime, database); err != nil {
				base.ErrorfCtx(bh.blipContextDb.Ctx, "Error from bh.handleChangesResponse: %v", err)
			}
		}(bh, sender, outrq.Response(), changeArray, sendTime, handleChangesResponseDb)
	} else {
		outrq.SetNoReply(true)
		if !bh.sendBLIPMessage(sender, outrq) {
			return ErrClosedBLIPSender
		}
	}

	if len(changeArray) > 0 {
		sequence := changeArray[0][0].(SequenceID)
		base.InfofCtx(bh.blipContextDb.Ctx, base.KeySync, "Sent %d changes to client, from seq %s", len(changeArray), sequence.String())
	} else {
		base.InfofCtx(bh.blipContextDb.Ctx, base.KeySync, "Sent all changes to client")
	}

	return nil
}

// Handles a "changes" request, i.e. a set of changes pushed by the client
func (bh *blipHandler) handleChanges(rq *blip.Message) error {
	if !bh.db.AllowConflicts() {
		return base.HTTPErrorf(http.StatusConflict, "Use 'proposeChanges' instead")
	}
	var changeList [][]interface{}
	if err := rq.ReadJSONBody(&changeList); err != nil {
		base.Warnf("Handle changes got error: %v", err)
		return err
	}

	bh.logEndpointEntry(rq.Profile(), fmt.Sprintf("#Changes:%d", len(changeList)))
	if len(changeList) == 0 {
		return nil
	}
	output := bytes.NewBuffer(make([]byte, 0, 100*len(changeList)))
	output.Write([]byte("["))
	jsonOutput := base.JSONEncoder(output)
	nWritten := 0

	// Include changes messages w/ proposeChanges stats, although CBL should only be using proposeChanges
	startTime := time.Now()
	bh.dbStats.CblReplicationPush().Add(base.StatKeyProposeChangeCount, int64(len(changeList)))
	defer func() {
		bh.dbStats.CblReplicationPush().Add(base.StatKeyProposeChangeTime, time.Since(startTime).Nanoseconds())
	}()

	expectedSeqs := make([]string, 0)

	for _, change := range changeList {
		docID := change[1].(string)
		revID := change[2].(string)
		missing, possible := bh.db.RevDiff(docID, []string{revID})
		if nWritten > 0 {
			output.Write([]byte(","))
		}
		if missing == nil {
			// already have this rev, tell the peer to skip sending it
			output.Write([]byte("0"))
		} else {
			// we want this rev, send possible ancestors to the peer
			if len(possible) == 0 {
				output.Write([]byte("[]"))
			} else {
				err := jsonOutput.Encode(possible)
				if err != nil {
					base.InfofCtx(bh.blipContextDb.Ctx, base.KeyAll, "Error encoding json: %v", err)
				}
			}

			// skip parsing seqno if we're not going to use it (no callback defined)
			if bh.postHandleChangesCallback != nil {
				var seqStr string
				switch seq := change[0].(type) {
				case string:
					seqStr = seq
				case json.Number:
					seqStr = seq.String()
				}
				expectedSeqs = append(expectedSeqs, seqStr)
			}
		}
		nWritten++
	}
	output.Write([]byte("]"))
	response := rq.Response()
	response.SetCompressed(true)
	response.SetBody(output.Bytes())

	if bh.postHandleChangesCallback != nil {
		bh.postHandleChangesCallback(expectedSeqs)
	}

	return nil
}

// Handles a "proposeChanges" request, similar to "changes" but in no-conflicts mode
func (bh *blipHandler) handleProposeChanges(rq *blip.Message) error {
	var changeList [][]interface{}
	if err := rq.ReadJSONBody(&changeList); err != nil {
		return err
	}
	bh.logEndpointEntry(rq.Profile(), fmt.Sprintf("#Changes: %d", len(changeList)))
	if len(changeList) == 0 {
		return nil
	}
	output := bytes.NewBuffer(make([]byte, 0, 5*len(changeList)))
	output.Write([]byte("["))
	nWritten := 0

	// proposeChanges stats
	startTime := time.Now()
	bh.dbStats.CblReplicationPush().Add(base.StatKeyProposeChangeCount, int64(len(changeList)))
	defer func() {
		bh.dbStats.CblReplicationPush().Add(base.StatKeyProposeChangeTime, time.Since(startTime).Nanoseconds())
	}()

	for i, change := range changeList {
		docID := change[0].(string)
		revID := change[1].(string)
		parentRevID := ""
		if len(change) > 2 {
			parentRevID = change[2].(string)
		}
		status := bh.db.CheckProposedRev(docID, revID, parentRevID)
		if status != 0 {
			// Skip writing trailing zeroes; but if we write a number afterwards we have to catch up
			if nWritten > 0 {
				output.Write([]byte(","))
			}
			for ; nWritten < i; nWritten++ {
				output.Write([]byte("0,"))
			}
			output.Write([]byte(strconv.FormatInt(int64(status), 10)))
			nWritten++
		}
	}
	output.Write([]byte("]"))
	response := rq.Response()
	if bh.sgCanUseDeltas {
		base.DebugfCtx(bh.blipContextDb.Ctx, base.KeyAll, "Setting deltas=true property on proposeChanges response")
		response.Properties[ChangesResponseDeltas] = "true"
	}
	response.SetCompressed(true)
	response.SetBody(output.Bytes())
	return nil
}

//////// DOCUMENTS:

func (bsc *BlipSyncContext) sendRevAsDelta(sender *blip.Sender, docID, revID, deltaSrcRevID string, seq SequenceID, knownRevs map[string]bool, maxHistory int, handleChangesResponseDb *Database) error {

	bsc.dbStats.StatsDeltaSync().Add(base.StatKeyDeltasRequested, 1)

	revDelta, redactedRev, err := handleChangesResponseDb.GetDelta(docID, deltaSrcRevID, revID)
	if err == ErrForbidden {
		return err
	} else if base.IsDeltaError(err) {
		// Something went wrong in the diffing library. We want to know about this!
		base.WarnfCtx(bsc.blipContextDb.Ctx, "Falling back to full body replication. Error generating delta from %s to %s for key %s - err: %v", deltaSrcRevID, revID, base.UD(docID), err)
		return bsc.sendRevision(sender, docID, revID, seq, knownRevs, maxHistory, handleChangesResponseDb)
	} else if err != nil {
		base.DebugfCtx(bsc.blipContextDb.Ctx, base.KeySync, "Falling back to full body replication. Couldn't get delta from %s to %s for key %s - err: %v", deltaSrcRevID, revID, base.UD(docID), err)
		return bsc.sendRevision(sender, docID, revID, seq, knownRevs, maxHistory, handleChangesResponseDb)
	}

	if redactedRev != nil {
		history := toHistory(redactedRev.History, knownRevs, maxHistory)
		properties := blipRevMessageProperties(history, redactedRev.Deleted, seq)
		return bsc.sendRevisionWithProperties(sender, docID, revID, redactedRev.BodyBytes, nil, properties)
	}

	if revDelta == nil {
		base.DebugfCtx(bsc.blipContextDb.Ctx, base.KeySync, "Falling back to full body replication. Couldn't get delta from %s to %s for key %s", deltaSrcRevID, revID, base.UD(docID))
		return bsc.sendRevision(sender, docID, revID, seq, knownRevs, maxHistory, handleChangesResponseDb)
	}

	base.TracefCtx(bsc.blipContextDb.Ctx, base.KeySync, "docID: %s - delta: %v", base.UD(docID), base.UD(string(revDelta.DeltaBytes)))
	if err := bsc.sendDelta(sender, docID, deltaSrcRevID, revDelta, seq); err != nil {
		return err
	}

	bsc.dbStats.StatsDeltaSync().Add(base.StatKeyDeltasSent, 1)

	return nil
}

func (bh *blipHandler) handleNoRev(rq *blip.Message) error {
	base.InfofCtx(bh.blipContextDb.Ctx, base.KeySyncMsg, "%s: norev for doc %q / %q - error: %q - reason: %q",
		rq.String(), base.UD(rq.Properties[NorevMessageId]), rq.Properties[NorevMessageRev], rq.Properties[NorevMessageError], rq.Properties[NorevMessageReason])

	// Couchbase Lite always sense noreply=true for norev profiles
	// but for testing purposes, it's useful to know which handler processed the message
	if !rq.NoReply() && rq.Properties[SGShowHandler] == "true" {
		response := rq.Response()
		response.Properties[SGHandler] = "handleNoRev"
	}

	return nil
}

// Received a "rev" request, i.e. client is pushing a revision body
func (bh *blipHandler) handleRev(rq *blip.Message) error {
	startTime := time.Now()
	defer func() {
		bh.dbStats.CblReplicationPush().Add(base.StatKeyWriteProcessingTime, time.Since(startTime).Nanoseconds())
	}()

	//addRevisionParams := newAddRevisionParams(rq)
	revMessage := RevMessage{Message: rq}

	base.DebugfCtx(bh.blipContextDb.Ctx, base.KeySyncMsg, "#%d: Type:%s %s", bh.serialNumber, rq.Profile(), revMessage.String())

	bodyBytes, err := rq.Body()
	if err != nil {
		return err
	}

	base.TracefCtx(bh.blipContextDb.Ctx, base.KeySyncMsg, "#%d: Properties:%v  Body:%s", bh.serialNumber, base.UD(revMessage.Properties), base.UD(string(bodyBytes)))

	bh.dbStats.StatsDatabase().Add(base.StatKeyDocWritesBytesBlip, int64(len(bodyBytes)))

	// Doc metadata comes from the BLIP message metadata, not magic document properties:
	docID, found := revMessage.ID()
	revID, rfound := revMessage.Rev()
	if !found || !rfound {
		return base.HTTPErrorf(http.StatusBadRequest, "Missing docID or revID")
	}

	newDoc := &Document{
		ID:    docID,
		RevID: revID,
	}
	newDoc.UpdateBodyBytes(bodyBytes)

	injectedAttachmentsForDelta := false
	if deltaSrcRevID, isDelta := revMessage.DeltaSrc(); isDelta {
		if !bh.sgCanUseDeltas {
			return base.HTTPErrorf(http.StatusBadRequest, "Deltas are disabled for this peer")
		}

		//  TODO: Doing a GetRevCopy here duplicates some rev cache retrieval effort, since deltaRevSrc is always
		//        going to be the current rev (no conflicts), and PutExistingRev will need to retrieve the
		//        current rev over again.  Should push this handling down PutExistingRev and use the version
		//        returned via callback in WriteUpdate, but blocked by moving attachment metadata to a rev property first
		//        (otherwise we don't have information needed to do downloadOrVerifyAttachments below prior to PutExistingRev)

		// Note: Using GetRevCopy here, and not direct rev cache retrieval, because it's still necessary to apply access check
		//       while retrieving deltaSrcRevID.  Couchbase Lite replication guarantees client has access to deltaSrcRevID,
		//       due to no-conflict write restriction, but we still need to enforce security here to prevent leaking data about previous
		//       revisions to malicious actors (in the scenario where that user has write but not read access).
		deltaSrcRev, err := bh.db.GetRev(docID, deltaSrcRevID, false, nil)
		if err != nil {
			return base.HTTPErrorf(http.StatusNotFound, "Can't fetch doc for deltaSrc=%s %v", deltaSrcRevID, err)
		}

		// Receiving a delta to be applied on top of a tombstone is not valid.
		if deltaSrcRev.Deleted {
			return base.HTTPErrorf(http.StatusNotFound, "Can't use delta. Found tombstone for deltaSrc=%s", deltaSrcRevID)
		}

		deltaSrcBody, err := deltaSrcRev.DeepMutableBody()
		if err != nil {
			return base.HTTPErrorf(http.StatusInternalServerError, "Unable to unmarshal mutable body for deltaSrc=%s %v", deltaSrcRevID, err)
		}

		// Stamp attachments so we can patch them
		if len(deltaSrcRev.Attachments) > 0 {
			deltaSrcBody[BodyAttachments] = map[string]interface{}(deltaSrcRev.Attachments)
			injectedAttachmentsForDelta = true
		}

		deltaSrcMap := map[string]interface{}(deltaSrcBody)
		err = base.Patch(&deltaSrcMap, newDoc.Body())
		if err != nil {
			// Something went wrong in the diffing library. We want to know about this!
			base.WarnfCtx(bh.blipContextDb.Ctx, "Error patching deltaSrc %s with %s for key %s with delta - err: %v", deltaSrcRevID, revID, base.UD(docID), err)
			return base.HTTPErrorf(http.StatusInternalServerError, "Error patching deltaSrc with delta: %s", err)
		}

		newDoc.UpdateBody(deltaSrcMap)
		base.TracefCtx(bh.blipContextDb.Ctx, base.KeySync, "docID: %s - body after patching: %v", base.UD(docID), base.UD(deltaSrcMap))
		bh.dbStats.StatsDeltaSync().Add(base.StatKeyDeltaPushDocCount, 1)
	}

	// Handle and pull out expiry
	if bytes.Contains(bodyBytes, []byte(BodyExpiry)) {
		body := newDoc.Body()
		expiry, err := body.ExtractExpiry()
		if err != nil {
			return base.HTTPErrorf(http.StatusBadRequest, "Invalid expiry: %v", err)
		}
		newDoc.DocExpiry = expiry
		newDoc.UpdateBody(body)
	}

	newDoc.Deleted = revMessage.Deleted()

	// noconflicts flag from LiteCore
	// https://github.com/couchbase/couchbase-lite-core/wiki/Replication-Protocol#rev
	var noConflicts bool
	if val, ok := rq.Properties[RevMessageNoConflicts]; ok {
		var err error
		noConflicts, err = strconv.ParseBool(val)
		if err != nil {
			return base.HTTPErrorf(http.StatusBadRequest, "Invalid value for noconflicts: %s", err)
		}
	}

	history := []string{revID}
	if historyStr := rq.Properties[RevMessageHistory]; historyStr != "" {
		history = append(history, strings.Split(historyStr, ",")...)
	}

	// Look at attachments with revpos > the last common ancestor's
	minRevpos := 1
	if len(history) > 0 {
		minRevpos, _ := ParseRevID(history[len(history)-1])
		minRevpos++
	}

	// Pull out attachments
	if injectedAttachmentsForDelta || bytes.Contains(bodyBytes, []byte(BodyAttachments)) {
		body := newDoc.Body()

		// Check for any attachments I don't have yet, and request them:
		if err := bh.downloadOrVerifyAttachments(rq.Sender, body, minRevpos, docID); err != nil {
			base.ErrorfCtx(bh.blipContextDb.Ctx, "Error during downloadOrVerifyAttachments for doc %s/%s: %v", base.UD(docID), revID, err)
			return err
		}

		newDoc.DocAttachments = GetBodyAttachments(body)
		delete(body, BodyAttachments)
		newDoc.UpdateBody(body)
	}

	// Finally, save the revision (with the new attachments inline)
	bh.dbStats.CblReplicationPush().Add(base.StatKeyDocPushCount, 1)

	_, _, err = bh.db.PutExistingRev(newDoc, history, noConflicts)
	if err != nil {
		return err
	}

	if bh.postHandleRevCallback != nil {
		bh.postHandleRevCallback(rq.Properties[RevMessageSequence])
	}

	return nil
}

//////// ATTACHMENTS:

// Received a "getAttachment" request
func (bh *blipHandler) handleGetAttachment(rq *blip.Message) error {

	getAttachmentParams := newGetAttachmentParams(rq)
	bh.logEndpointEntry(rq.Profile(), getAttachmentParams.String())

	digest := getAttachmentParams.digest()
	if digest == "" {
		return base.HTTPErrorf(http.StatusBadRequest, "Missing 'digest'")
	}
	if !bh.isAttachmentAllowed(digest) {
		return base.HTTPErrorf(http.StatusForbidden, "Attachment's doc not being synced")
	}
	attachment, err := bh.db.GetAttachment(AttachmentKey(digest))
	if err != nil {
		return err

	}
	base.DebugfCtx(bh.blipContextDb.Ctx, base.KeySync, "Sending attachment with digest=%q (%dkb)", digest, len(attachment)/1024)
	response := rq.Response()
	response.SetBody(attachment)
	response.SetCompressed(rq.Properties[BlipCompress] == "true")
	bh.dbStats.StatsCblReplicationPull().Add(base.StatKeyAttachmentPullCount, 1)
	bh.dbStats.StatsCblReplicationPull().Add(base.StatKeyAttachmentPullBytes, int64(len(attachment)))

	return nil
}

// For each attachment in the revision, makes sure it's in the database, asking the client to
// upload it if necessary. This method blocks until all the attachments have been processed.
func (bh *blipHandler) downloadOrVerifyAttachments(sender *blip.Sender, body Body, minRevpos int, docID string) error {
	return bh.db.ForEachStubAttachment(body, minRevpos,
		func(name string, digest string, knownData []byte, meta map[string]interface{}) ([]byte, error) {
			if knownData != nil {
				// If I have the attachment already I don't need the client to send it, but for
				// security purposes I do need the client to _prove_ it has the data, otherwise if
				// it knew the digest it could acquire the data by uploading a document with the
				// claimed attachment, then downloading it.
				base.DebugfCtx(bh.blipContextDb.Ctx, base.KeySync, "    Verifying attachment %q for doc %s (digest %s)", base.UD(name), base.UD(docID), digest)
				nonce, proof := GenerateProofOfAttachment(knownData)
				outrq := blip.NewRequest()
				outrq.Properties = map[string]string{BlipProfile: MessageProveAttachment, ProveAttachmentDigest: digest}
				outrq.SetBody(nonce)
				if !bh.sendBLIPMessage(sender, outrq) {
					return nil, ErrClosedBLIPSender
				}
				if body, err := outrq.Response().Body(); err != nil {
					base.WarnfCtx(bh.blipContextDb.Ctx, "Error returned for proveAttachment message for doc %s (digest %s).  Error: %v", base.UD(docID), digest, err)
					return nil, err
				} else if string(body) != proof {
					base.WarnfCtx(bh.blipContextDb.Ctx, "Incorrect proof for attachment %s : I sent nonce %x, expected proof %q, got %q", digest, base.MD(nonce), base.MD(proof), base.MD(string(body)))
					return nil, base.HTTPErrorf(http.StatusForbidden, "Incorrect proof for attachment %s", digest)
				} else {
					base.InfofCtx(bh.blipContextDb.Ctx, base.KeySync, "proveAttachment successful for doc %s (digest %s)", base.UD(docID), digest)
				}
				return nil, nil
			} else {
				// If I don't have the attachment, I will request it from the client:
				base.DebugfCtx(bh.blipContextDb.Ctx, base.KeySync, "    Asking for attachment %q for doc %s (digest %s)", base.UD(name), base.UD(docID), digest)
				outrq := blip.NewRequest()
				outrq.Properties = map[string]string{BlipProfile: MessageGetAttachment, GetAttachmentDigest: digest}
				if isCompressible(name, meta) {
					outrq.Properties[BlipCompress] = "true"
				}
				if !bh.sendBLIPMessage(sender, outrq) {
					return nil, ErrClosedBLIPSender
				}
				attBody, err := outrq.Response().Body()
				if err != nil {
					return nil, err
				}

				lNum, metaLengthOK := meta["length"].(json.Number)
				metaLength, err := lNum.Int64()
				if err != nil {
					return nil, err
				}

				// Verify that the attachment we received matches the metadata stored in the document
				if !metaLengthOK || len(attBody) != int(metaLength) || Sha1DigestKey(attBody) != digest {
					return nil, base.HTTPErrorf(http.StatusBadRequest, "Incorrect data sent for attachment with digest: %s", digest)
				}

				return attBody, nil
			}
		})
}

func (bsc *BlipSyncContext) incrementSerialNumber() uint64 {
	return atomic.AddUint64(&bsc.handlerSerialNumber, 1)
}

func (bsc *BlipSyncContext) addAllowedAttachments(attDigests []string) {
	bsc.lock.Lock()
	defer bsc.lock.Unlock()
	if bsc.allowedAttachments == nil {
		bsc.allowedAttachments = make(map[string]int, 100)
	}
	for _, digest := range attDigests {
		bsc.allowedAttachments[digest] = bsc.allowedAttachments[digest] + 1
	}
	base.TracefCtx(bsc.blipContextDb.Ctx, base.KeySync, "addAllowedAttachments, added: %v current set: %v", attDigests, bsc.allowedAttachments)
}

func (bsc *BlipSyncContext) removeAllowedAttachments(attDigests []string) {
	bsc.lock.Lock()
	defer bsc.lock.Unlock()
	for _, digest := range attDigests {
		if n := bsc.allowedAttachments[digest]; n > 1 {
			bsc.allowedAttachments[digest] = n - 1
		} else {
			delete(bsc.allowedAttachments, digest)
		}
	}

	base.TracefCtx(bsc.blipContextDb.Ctx, base.KeySync, "removeAllowedAttachments, removed: %v current set: %v", attDigests, bsc.allowedAttachments)
}

func (bh *blipHandler) logEndpointEntry(profile, endpoint string) {
	base.InfofCtx(bh.blipContextDb.Ctx, base.KeySyncMsg, "#%d: Type:%s %s", bh.serialNumber, profile, endpoint)
}
