// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/disttask/framework/dispatcher"
	"github.com/pingcap/tidb/pkg/disttask/framework/mock"
	mockexecute "github.com/pingcap/tidb/pkg/disttask/framework/mock/execute"
	"github.com/pingcap/tidb/pkg/disttask/framework/proto"
	"github.com/pingcap/tidb/pkg/disttask/framework/scheduler"
	"github.com/pingcap/tidb/pkg/disttask/framework/storage"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// RegisterTaskMeta initialize mock components for dist task.
func RegisterTaskMeta(t *testing.T, ctrl *gomock.Controller, dispatcherHandle dispatcher.Extension, testContext *TestContext, runSubtaskFn func(ctx context.Context, subtask *proto.Subtask) error) {
	mockExtension := mock.NewMockExtension(ctrl)
	mockCleanupRountine := mock.NewMockCleanUpRoutine(ctrl)
	mockCleanupRountine.EXPECT().CleanUp(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSubtaskExecutor := GetMockSubtaskExecutor(ctrl)
	if runSubtaskFn == nil {
		mockSubtaskExecutor.EXPECT().RunSubtask(gomock.Any(), gomock.Any()).DoAndReturn(
			func(ctx context.Context, subtask *proto.Subtask) error {
				switch subtask.Step {
				case proto.StepOne:
					testContext.M.Store("0", "0")
				case proto.StepTwo:
					testContext.M.Store("1", "1")
				default:
					panic("invalid step")
				}
				return nil
			}).AnyTimes()
	} else {
		mockSubtaskExecutor.EXPECT().RunSubtask(gomock.Any(), gomock.Any()).DoAndReturn(runSubtaskFn).AnyTimes()
	}
	mockExtension.EXPECT().IsIdempotent(gomock.Any()).Return(true).AnyTimes()
	mockExtension.EXPECT().GetSubtaskExecutor(gomock.Any(), gomock.Any(), gomock.Any()).Return(mockSubtaskExecutor, nil).AnyTimes()
	registerTaskMetaInner(t, proto.TaskTypeExample, mockExtension, mockCleanupRountine, dispatcherHandle)
}

func registerTaskMetaInner(t *testing.T, taskType proto.TaskType, mockExtension scheduler.Extension, mockCleanup dispatcher.CleanUpRoutine, dispatcherHandle dispatcher.Extension) {
	t.Cleanup(func() {
		dispatcher.ClearDispatcherFactory()
		dispatcher.ClearDispatcherCleanUpFactory()
		scheduler.ClearSchedulers()
	})
	dispatcher.RegisterDispatcherFactory(taskType,
		func(ctx context.Context, taskMgr dispatcher.TaskManager, serverID string, task *proto.Task) dispatcher.Dispatcher {
			baseDispatcher := dispatcher.NewBaseDispatcher(ctx, taskMgr, serverID, task)
			baseDispatcher.Extension = dispatcherHandle
			return baseDispatcher
		})

	dispatcher.RegisterDispatcherCleanUpFactory(taskType,
		func() dispatcher.CleanUpRoutine {
			return mockCleanup
		})

	scheduler.RegisterTaskType(taskType,
		func(ctx context.Context, id string, task *proto.Task, taskTable scheduler.TaskTable) scheduler.Scheduler {
			s := scheduler.NewBaseScheduler(ctx, id, task.ID, taskTable)
			s.Extension = mockExtension
			return s
		},
	)
}

// RegisterRollbackTaskMeta register rollback task meta.
func RegisterRollbackTaskMeta(t *testing.T, ctrl *gomock.Controller, mockDispatcher dispatcher.Extension, testContext *TestContext) {
	mockExtension := mock.NewMockExtension(ctrl)
	mockExecutor := mockexecute.NewMockSubtaskExecutor(ctrl)
	mockCleanupRountine := mock.NewMockCleanUpRoutine(ctrl)
	mockCleanupRountine.EXPECT().CleanUp(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockExecutor.EXPECT().Init(gomock.Any()).Return(nil).AnyTimes()
	mockExecutor.EXPECT().Cleanup(gomock.Any()).Return(nil).AnyTimes()
	mockExecutor.EXPECT().Rollback(gomock.Any()).DoAndReturn(
		func(_ context.Context) error {
			testContext.RollbackCnt.Add(1)
			return nil
		},
	).AnyTimes()
	mockExecutor.EXPECT().RunSubtask(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ *proto.Subtask) error {
			testContext.M.Store("1", "1")
			return nil
		}).AnyTimes()
	mockExecutor.EXPECT().OnFinished(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockExtension.EXPECT().IsIdempotent(gomock.Any()).Return(true).AnyTimes()
	mockExtension.EXPECT().GetSubtaskExecutor(gomock.Any(), gomock.Any(), gomock.Any()).Return(mockExecutor, nil).AnyTimes()

	registerTaskMetaInner(t, proto.TaskTypeExample, mockExtension, mockCleanupRountine, mockDispatcher)
	testContext.RollbackCnt.Store(0)
}

// DispatchTask dispatch one task.
func DispatchTask(ctx context.Context, t *testing.T, taskKey string) *proto.Task {
	mgr, err := storage.GetTaskManager()
	require.NoError(t, err)
	_, err = mgr.AddNewGlobalTask(ctx, taskKey, proto.TaskTypeExample, 8, nil)
	require.NoError(t, err)
	return WaitTaskExit(ctx, t, taskKey)
}

// WaitTaskExit wait until the task exit.
func WaitTaskExit(ctx context.Context, t *testing.T, taskKey string) *proto.Task {
	mgr, err := storage.GetTaskManager()
	require.NoError(t, err)
	var task *proto.Task
	start := time.Now()
	for {
		if time.Since(start) > 10*time.Minute {
			require.FailNow(t, "timeout")
		}

		time.Sleep(time.Second)
		task, err = mgr.GetGlobalTaskByKeyWithHistory(ctx, taskKey)
		require.NoError(t, err)
		require.NotNil(t, task)
		if task.State != proto.TaskStatePending && task.State != proto.TaskStateRunning && task.State != proto.TaskStateCancelling && task.State != proto.TaskStateReverting && task.State != proto.TaskStatePausing {
			break
		}
	}
	return task
}

// DispatchTaskAndCheckSuccess dispatch one task and check if it is succeed.
func DispatchTaskAndCheckSuccess(ctx context.Context, t *testing.T, taskKey string, testContext *TestContext, checkResultFn func(t *testing.T, testContext *TestContext)) {
	task := DispatchTask(ctx, t, taskKey)
	require.Equal(t, proto.TaskStateSucceed, task.State)
	if checkResultFn == nil {
		v, ok := testContext.M.Load("1")
		require.Equal(t, true, ok)
		require.Equal(t, "1", v)
		v, ok = testContext.M.Load("0")
		require.Equal(t, true, ok)
		require.Equal(t, "0", v)
		return
	}
	checkResultFn(t, testContext)
	testContext.M = sync.Map{}
}

// DispatchAndCancelTask dispatch one task then cancel it.
func DispatchAndCancelTask(ctx context.Context, t *testing.T, taskKey string, testContext *TestContext) {
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/disttask/framework/scheduler/MockExecutorRunCancel", "1*return(1)"))
	defer func() {
		require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/disttask/framework/scheduler/MockExecutorRunCancel"))
	}()
	task := DispatchTask(ctx, t, taskKey)
	require.Equal(t, proto.TaskStateReverted, task.State)
	testContext.M.Range(func(key, value interface{}) bool {
		testContext.M.Delete(key)
		return true
	})
}

// DispatchTaskAndCheckState dispatch one task and check the task state.
func DispatchTaskAndCheckState(ctx context.Context, t *testing.T, taskKey string, testContext *TestContext, state proto.TaskState) {
	task := DispatchTask(ctx, t, taskKey)
	require.Equal(t, state, task.State)
	testContext.M.Range(func(key, value interface{}) bool {
		testContext.M.Delete(key)
		return true
	})
}

// DispatchMultiTasksAndOneFail dispatches multiple tasks and force one task failed.
// TODO(ywqzzy): run tasks with multiple types.
func DispatchMultiTasksAndOneFail(ctx context.Context, t *testing.T, num int, testContext *TestContext) {
	mgr, err := storage.GetTaskManager()
	require.NoError(t, err)
	tasks := make([]*proto.Task, num)
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/pkg/disttask/framework/scheduler/MockExecutorRunErr", "1*return(true)"))

	for i := 0; i < num; i++ {
		_, err = mgr.AddNewGlobalTask(ctx, fmt.Sprintf("key%d", i), proto.TaskTypeExample, 8, nil)
		require.NoError(t, err)
	}
	for i := 0; i < num; i++ {
		tasks[i] = WaitTaskExit(ctx, t, fmt.Sprintf("key%d", i))
	}
	require.Equal(t, proto.TaskStateReverted, tasks[0].State)
	require.Equal(t, proto.TaskStateSucceed, tasks[1].State)
	require.Equal(t, proto.TaskStateSucceed, tasks[2].State)

	testContext.M.Range(func(key, value interface{}) bool {
		testContext.M.Delete(key)
		return true
	})
	require.NoError(t, failpoint.Disable("github.com/pingcap/tidb/pkg/disttask/framework/scheduler/MockExecutorRunErr"))
}
