// Copyright 2022-Present Couchbase, Inc.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
// in that file, in accordance with the Business Source License, use of this
// software will be governed by the Apache License, Version 2.0, included in
// the file licenses/APL2.txt.

package attachmentcompactiontest

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/couchbase/gocbcore/v10"
	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/db"
	"github.com/couchbase/sync_gateway/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentCompactionAPI(t *testing.T) {

	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}

	// attachment compaction has to run on default collection, we can't run on multiple scopes right now for SG_TEST_USE_DEFAULT_COLLECTION = false
	rt := rest.NewRestTesterDefaultCollection(t, nil)
	defer rt.Close()

	// cleanup attachments left behind
	defer func() {
		resp := rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&reset=true", "")
		rest.RequireStatus(t, resp, http.StatusOK)
		_ = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)
	}()
	// Perform GET before compact has been ran, ensure it starts in valid 'stopped' state
	resp := rt.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	var response db.AttachmentManagerResponse
	err := base.JSONUnmarshal(resp.BodyBytes(), &response)
	require.NoError(t, err)
	require.Equal(t, db.BackgroundProcessStateCompleted, response.State)
	require.Equal(t, int64(0), response.MarkedAttachments)
	require.Equal(t, int64(0), response.PurgedAttachments)
	require.Empty(t, response.LastErrorMessage)

	// Kick off compact
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	// Attempt to kick off again and validate it correctly errors
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusServiceUnavailable)

	// Wait for run to complete
	err = rt.WaitForCondition(func() bool {
		time.Sleep(1 * time.Second)

		resp := rt.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
		rest.RequireStatus(t, resp, http.StatusOK)

		var response db.AttachmentManagerResponse
		err = base.JSONUnmarshal(resp.BodyBytes(), &response)
		require.NoError(t, err)

		return response.State == db.BackgroundProcessStateCompleted
	})
	require.NoError(t, err)

	dataStore := rt.GetSingleDataStore()
	collection := &db.DatabaseCollectionWithUser{
		DatabaseCollection: rt.GetSingleTestDatabaseCollection(),
	}
	// Create some legacy attachments to be marked but not compacted
	ctx := rt.Context()
	for i := 0; i < 20; i++ {
		docID := fmt.Sprintf("testDoc-%d", i)
		attID := fmt.Sprintf("testAtt-%d", i)
		attBody := map[string]interface{}{"value": strconv.Itoa(i)}
		attJSONBody, err := base.JSONMarshal(attBody)
		require.NoError(t, err)
		rest.CreateLegacyAttachmentDoc(t, ctx, collection, dataStore, docID, []byte("{}"), attID, attJSONBody)
	}

	// Create some 'unmarked' attachments
	makeUnmarkedDoc := func(docid string) {
		err := dataStore.SetRaw(docid, 0, nil, []byte("{}"))
		require.NoError(t, err)
	}

	for i := 0; i < 5; i++ {
		docID := fmt.Sprintf("%s%s%d", base.AttPrefix, "unmarked", i)
		makeUnmarkedDoc(docID)
	}

	// Start attachment compaction run
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	// Wait for run to complete
	err = rt.WaitForCondition(func() bool {
		time.Sleep(1 * time.Second)

		resp := rt.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
		rest.RequireStatus(t, resp, http.StatusOK)

		var response db.AttachmentManagerResponse
		err = base.JSONUnmarshal(resp.BodyBytes(), &response)
		require.NoError(t, err)

		return response.State == db.BackgroundProcessStateCompleted
	})
	require.NoError(t, err)

	// Validate results of GET
	resp = rt.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	err = base.JSONUnmarshal(resp.BodyBytes(), &response)
	require.NoError(t, err)
	require.Equal(t, db.BackgroundProcessStateCompleted, response.State)
	require.Equal(t, int64(20), response.MarkedAttachments)
	require.Equal(t, int64(5), response.PurgedAttachments)
	require.Empty(t, response.LastErrorMessage)

	// Start another run
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	// Attempt to terminate that run
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&action=stop", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	// Verify it has been marked as 'stopping' --> its possible we'll get stopped instead based on timing of persisted doc update
	err = rt.WaitForCondition(func() bool {
		time.Sleep(1 * time.Second)

		resp := rt.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
		rest.RequireStatus(t, resp, http.StatusOK)

		var response db.AttachmentManagerResponse
		err = base.JSONUnmarshal(resp.BodyBytes(), &response)
		require.NoError(t, err)

		return response.State == db.BackgroundProcessStateStopping || response.State == db.BackgroundProcessStateStopped
	})
	require.NoError(t, err)

	// Wait for run to complete
	_ = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateStopped)
}

func TestAttachmentCompactionPersistence(t *testing.T) {
	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}

	tb := base.GetTestBucket(t)
	noCloseTB := tb.NoCloseClone()

	rt1 := rest.NewRestTester(t, &rest.RestTesterConfig{
		CustomTestBucket: noCloseTB,
	})
	rt2 := rest.NewRestTester(t, &rest.RestTesterConfig{
		CustomTestBucket: tb,
	})
	defer rt2.Close()
	defer rt1.Close()

	// Start attachment compaction on one SGW
	resp := rt1.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	_ = rt1.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)

	// Ensure compaction is marked complete on the other node too
	var rt2AttachmentStatus db.AttachmentManagerResponse
	resp = rt2.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	err := base.JSONUnmarshal(resp.BodyBytes(), &rt2AttachmentStatus)
	assert.NoError(t, err)
	assert.Equal(t, rt2AttachmentStatus.State, db.BackgroundProcessStateCompleted)

	// Start compaction again
	resp = rt1.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status := rt1.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateRunning)
	compactID := status.CompactID

	// Abort process early from rt1
	resp = rt1.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&action=stop", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status = rt2.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateStopped)

	// Ensure aborted status is present on rt2
	resp = rt2.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	err = base.JSONUnmarshal(resp.BodyBytes(), &rt2AttachmentStatus)
	assert.NoError(t, err)
	assert.Equal(t, db.BackgroundProcessStateStopped, rt2AttachmentStatus.State)

	// Attempt to start again from rt2 --> Should resume based on aborted state (same compactionID)
	resp = rt2.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status = rt2.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateRunning)
	assert.Equal(t, compactID, status.CompactID)

	// Wait for compaction to complete
	_ = rt1.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)
}

func TestAttachmentCompactionDryRun(t *testing.T) {
	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}

	// attachment compaction has to run on default collection, we can't run on multiple scopes right now for SG_TEST_USE_DEFAULT_COLLECTION = false
	rt := rest.NewRestTesterDefaultCollection(t, nil)
	defer rt.Close()

	dataStore := rt.GetSingleDataStore()
	// Create some 'unmarked' attachments
	makeUnmarkedDoc := func(docid string) {
		err := dataStore.SetRaw(docid, 0, nil, []byte("{}"))
		assert.NoError(t, err)
	}

	attachmentKeys := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		docID := fmt.Sprintf("%s%s%d", base.AttPrefix, "unmarked", i)
		makeUnmarkedDoc(docID)
		attachmentKeys = append(attachmentKeys, docID)
	}

	resp := rt.SendAdminRequest("POST", "/db/_compact?type=attachment&dry_run=true", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status := rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)
	assert.True(t, status.DryRun)
	assert.Equal(t, int64(5), status.PurgedAttachments)

	for _, docID := range attachmentKeys {
		_, _, err := dataStore.GetRaw(docID)
		assert.NoError(t, err)
	}

	resp = rt.SendAdminRequest("POST", "/db/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)
	assert.False(t, status.DryRun)
	assert.Equal(t, int64(5), status.PurgedAttachments)

	for _, docID := range attachmentKeys {
		_, _, err := dataStore.GetRaw(docID)
		assert.Error(t, err)
		assert.True(t, base.IsDocNotFoundError(err))
	}
}

func TestAttachmentCompactionReset(t *testing.T) {
	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}

	rt := rest.NewRestTester(t, nil)
	defer rt.Close()

	// Start compaction
	resp := rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status := rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateRunning)
	compactID := status.CompactID

	// Stop compaction before complete -- enters aborted state
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&action=stop", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateStopped)

	// Ensure status is aborted
	resp = rt.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	var attachmentStatus db.AttachmentManagerResponse
	err := base.JSONUnmarshal(resp.BodyBytes(), &attachmentStatus)
	assert.NoError(t, err)
	assert.Equal(t, db.BackgroundProcessStateStopped, attachmentStatus.State)

	// Start compaction again but with reset=true --> meaning it shouldn't try to resume
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&reset=true", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateRunning)
	assert.NotEqual(t, compactID, status.CompactID)

	// Wait for completion before closing test
	_ = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)
}

func TestAttachmentCompactionInvalidDocs(t *testing.T) {
	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}

	// attachment compaction has to run on default collection, we can't run on multiple scopes right now for SG_TEST_USE_DEFAULT_COLLECTION = false
	rt := rest.NewRestTesterDefaultCollection(t, nil)
	defer rt.Close()
	ctx := rt.Context()

	dataStore := rt.GetSingleDataStore()
	// Create a raw binary doc
	_, err := dataStore.AddRaw("binary", 0, []byte("binary doc"))
	assert.NoError(t, err)

	// Create a CBS tombstone
	_, err = dataStore.AddRaw("deleted", 0, []byte("{}"))
	assert.NoError(t, err)
	err = dataStore.Delete("deleted")
	assert.NoError(t, err)

	collection := &db.DatabaseCollectionWithUser{
		DatabaseCollection: rt.GetSingleTestDatabaseCollection(),
	}

	// Also create an actual legacy attachment to ensure they are still processed
	rest.CreateLegacyAttachmentDoc(t, ctx, collection, dataStore, "docID", []byte("{}"), "attKey", []byte("{}"))

	// Create attachment with no doc reference
	err = dataStore.SetRaw(base.AttPrefix+"test", 0, nil, []byte("{}"))
	assert.NoError(t, err)
	err = dataStore.SetRaw(base.AttPrefix+"test2", 0, nil, []byte("{}"))
	assert.NoError(t, err)

	// Write a normal doc to ensure this passes through fine
	resp := rt.SendAdminRequest("PUT", "/db/normal-doc", "{}")
	rest.RequireStatus(t, resp, http.StatusCreated)

	// Start compaction
	resp = rt.SendAdminRequest("POST", "/db/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status := rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)

	assert.Equal(t, int64(2), status.PurgedAttachments)
	assert.Equal(t, int64(1), status.MarkedAttachments)
	assert.Equal(t, db.BackgroundProcessStateCompleted, status.State)
}

func TestAttachmentCompactionStartTimeAndStats(t *testing.T) {
	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}

	rt := rest.NewRestTester(t, nil)
	defer rt.Close()

	// Create attachment with no doc reference
	err := rt.GetDatabase().Bucket.DefaultDataStore().SetRaw(base.AttPrefix+"test", 0, nil, []byte("{}"))
	assert.NoError(t, err)

	databaseStats := rt.GetDatabase().DbStats.Database()

	// Start compaction
	resp := rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status := rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)

	// Check stats and start time response is correct
	firstStartTime := status.StartTime
	firstStartTimeStat := databaseStats.CompactionAttachmentStartTime.Value()
	assert.False(t, firstStartTime.IsZero())
	assert.NotEqual(t, 0, firstStartTimeStat)
	assert.Equal(t, int64(1), databaseStats.NumAttachmentsCompacted.Value())

	// Start compaction again
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	status = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)

	// Check that stats have been updated to new run and previous attachment count stat remains
	assert.True(t, status.StartTime.After(firstStartTime))
	assert.True(t, databaseStats.CompactionAttachmentStartTime.Value() > firstStartTimeStat)
	assert.Equal(t, int64(1), databaseStats.NumAttachmentsCompacted.Value())
}

func TestAttachmentCompactionAbort(t *testing.T) {
	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}

	rt := rest.NewRestTester(t, nil)
	defer rt.Close()
	ctx := rt.Context()

	dataStore := rt.GetSingleDataStore()
	collection := &db.DatabaseCollectionWithUser{
		DatabaseCollection: rt.GetSingleTestDatabaseCollection(),
	}
	for i := 0; i < 1000; i++ {
		docID := fmt.Sprintf("testDoc-%d", i)
		attID := fmt.Sprintf("testAtt-%d", i)
		attBody := map[string]interface{}{"value": strconv.Itoa(i)}
		attJSONBody, err := base.JSONMarshal(attBody)
		assert.NoError(t, err)
		rest.CreateLegacyAttachmentDoc(t, ctx, collection, dataStore, docID, []byte("{}"), attID, attJSONBody)
	}

	resp := rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&action=stop", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	status := rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateStopped)
	assert.Equal(t, int64(0), status.PurgedAttachments)
}

func TestAttachmentCompactionMarkPhaseRollback(t *testing.T) {
	if base.UnitTestUrlIsWalrus() {
		t.Skip("This test only works against Couchbase Server")
	}
	var garbageVBUUID gocbcore.VbUUID = 1234
	base.SetUpTestLogging(t, base.LevelInfo, base.KeyAll)

	rt := rest.NewRestTesterDefaultCollection(t, nil)
	defer rt.Close()
	dataStore := rt.GetSingleDataStore()

	// Create some 'unmarked' attachments
	makeUnmarkedDoc := func(docid string) {
		err := dataStore.SetRaw(docid, 0, nil, []byte("{}"))
		require.NoError(t, err)
	}

	for i := 0; i < 1000; i++ {
		docID := fmt.Sprintf("%s%s%d", base.AttPrefix, "unmarked", i)
		makeUnmarkedDoc(docID)
	}

	// kick off compaction and wait for "mark" phase to begin
	resp := rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	_ = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateRunning)

	// immediately stop the compaction process (we just need the status data to be persisted to the bucket)
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&action=stop", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	stat := rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateStopped)
	require.Equal(t, db.MarkPhase, stat.Phase)

	// alter persisted dcp metadata from the first run to force a rollback
	name := db.GenerateCompactionDCPStreamName(stat.CompactID, "mark")
	checkpointPrefix := fmt.Sprintf("%s:%v", "_sync:dcp_ck:", name)

	meta := base.NewDCPMetadataCS(rt.Context(), dataStore, 1024, 8, checkpointPrefix)
	vbMeta := meta.GetMeta(0)
	vbMeta.VbUUID = garbageVBUUID
	meta.SetMeta(0, vbMeta)
	meta.Persist(rt.Context(), 0, []uint16{0})

	// kick off a new run attempting to start it again (should force into rollback handling)
	resp = rt.SendAdminRequest("POST", "/{{.db}}/_compact?type=attachment&action=start", "")
	rest.RequireStatus(t, resp, http.StatusOK)
	_ = rt.WaitForAttachmentCompactionStatus(t, db.BackgroundProcessStateCompleted)

	// Validate results of recovered attachment compaction process
	resp = rt.SendAdminRequest("GET", "/{{.db}}/_compact?type=attachment", "")
	rest.RequireStatus(t, resp, http.StatusOK)

	// validate that the compaction process actually recovered from rollback by checking stats
	var response db.AttachmentManagerResponse
	err := base.JSONUnmarshal(resp.BodyBytes(), &response)
	require.NoError(t, err)
	require.Equal(t, db.BackgroundProcessStateCompleted, response.State)
	require.Equal(t, int64(0), response.MarkedAttachments)
	require.Equal(t, int64(1000), response.PurgedAttachments)

}
