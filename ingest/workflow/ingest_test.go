package workflow_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/flint-fhir/flint/ingest/activity"
	"github.com/flint-fhir/flint/ingest/workflow"
)

func TestIngestFHIRBundle_HappyPath(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	a := &activity.Activities{}
	env.RegisterActivity(a)
	env.OnActivity(a.WriteMedplumBatch, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.WritePostgresBatch, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.PublishToAutoMQ, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(workflow.IngestFHIRBundle, workflow.IngestInput{
		TenantID:       "test-tenant",
		BundleID:       "bundle-001",
		ResourceProtos: [][]byte{{1, 2, 3}, {4, 5, 6}},
		ResourceTypes:  []string{"Patient", "Encounter"},
		ResourceIDs:    []string{"pat-1", "enc-1"},
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	t.Log("IngestFHIRBundle completed successfully")
}

func TestIngestFHIRBundle_MedplumFailure_ContinuesAnyway(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	a := &activity.Activities{}
	env.RegisterActivity(a)
	// Medplum fails
	env.OnActivity(a.WriteMedplumBatch, mock.Anything, mock.Anything).Return(fmt.Errorf("medplum is down"))
	// Postgres and AutoMQ succeed
	env.OnActivity(a.WritePostgresBatch, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.PublishToAutoMQ, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(workflow.IngestFHIRBundle, workflow.IngestInput{
		TenantID: "test-tenant", BundleID: "bundle-002",
		ResourceProtos: [][]byte{{1}}, ResourceTypes: []string{"Patient"}, ResourceIDs: []string{"pat-1"},
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("should succeed despite Medplum failure: %v", err)
	}
	t.Log("Workflow continued past Medplum failure")
}

func TestIngestFHIRBundle_PostgresFailure_Fails(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	a := &activity.Activities{}
	env.RegisterActivity(a)
	env.OnActivity(a.WriteMedplumBatch, mock.Anything, mock.Anything).Return(nil)
	// Postgres fails
	env.OnActivity(a.WritePostgresBatch, mock.Anything, mock.Anything).Return(fmt.Errorf("connection refused"))
	env.OnActivity(a.PublishToAutoMQ, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(workflow.IngestFHIRBundle, workflow.IngestInput{
		TenantID: "test-tenant", BundleID: "bundle-003",
		ResourceProtos: [][]byte{{1}}, ResourceTypes: []string{"Patient"}, ResourceIDs: []string{"pat-1"},
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("should fail when Postgres fails")
	} else {
		t.Logf("Correctly failed: %v", err)
	}
}

func TestIngestFHIRBundle_AutoMQFailure_Fails(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	a := &activity.Activities{}
	env.RegisterActivity(a)
	env.OnActivity(a.WriteMedplumBatch, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.WritePostgresBatch, mock.Anything, mock.Anything).Return(nil)
	// AutoMQ fails
	env.OnActivity(a.PublishToAutoMQ, mock.Anything, mock.Anything).Return(fmt.Errorf("broker unreachable"))

	env.ExecuteWorkflow(workflow.IngestFHIRBundle, workflow.IngestInput{
		TenantID: "test-tenant", BundleID: "bundle-004",
		ResourceProtos: [][]byte{{1}}, ResourceTypes: []string{"Patient"}, ResourceIDs: []string{"pat-1"},
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow not completed")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("should fail when AutoMQ fails")
	} else {
		t.Logf("Correctly failed: %v", err)
	}
}
