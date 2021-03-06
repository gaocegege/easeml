package workers

import (
	"bufio"
	"github.com/ds3lab/easeml/database/model"
	"github.com/ds3lab/easeml/modules"
	"github.com/ds3lab/easeml/storage"
	"io/ioutil"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// TaskRunListener periodically checks if there are any tasks which are in the "scheduled" state
// which means they are ready to run.
func (context Context) TaskRunListener() {
	for {
		task, err := context.ModelContext.LockTask(model.F{"status": model.TaskScheduled}, context.ProcessID, "", "")
		if err == nil {

			// Mark the process as working.
			context.repeatUntilSuccess(func() (err error) {
				_, err = context.ModelContext.SetProcessStatus(context.ProcessID, model.ProcWorking)
				return
			})

			log.Printf("TASK FOUND FOR EXECUTION")
			context.TaskRunWorker(task)

			// Mark the process as idle.
			context.repeatUntilSuccess(func() (err error) {
				_, err = context.ModelContext.SetProcessStatus(context.ProcessID, model.ProcIdle)
				return
			})

		} else if errors.Cause(err) == model.ErrNotFound {
			time.Sleep(context.Period)
		} else {
			panic(err)
		}
	}
}

// TaskRunWorker takes a task and runs it through all the stages.
func (context Context) TaskRunWorker(task model.Task) {

	// Mark the task as running.
	context.repeatUntilSuccess(func() error {
		return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskRunning, "")
	})

	// Dataset path.
	datasetPath, err := context.StorageContext.GetDatasetPath(task.Dataset, "")
	if err != nil {
		panic(err) // This means that we cannot access the file system.
	}

	// Parameters, predictions and evaluations path.
	paths, err := context.StorageContext.GetAllTaskPaths(task.ID)
	if err != nil {
		panic(err) // This means that we cannot access the file system.
	}

	// Ensure task model is loaded. Only needed if the task didn't arrive to the evaluation stage.
	var modelImageName string
	if task.Stage != model.TaskStageEvaluating {
		modelImageFilePath := context.getModuleImagePath(task.Model, model.ModuleModel)
		var err error
		modelImageName, err = modules.LoadImage(modelImageFilePath)
		if err != nil {
			err = errors.WithStack(err)
			context.Logger.WithFields(
				"module-id", task.Model,
				"task-id", task.ID,
			).WithStack(err).WithError(err).WriteError("MODEL LOAD ERROR")

			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
			})
			return
		}
	}

	// Put the task in the training stage.
	if task.Stage == model.TaskStageBegin {
		context.repeatUntilSuccess(func() error {
			return context.ModelContext.UpdateTaskStage(task.ID, model.TaskStageTraining)
		})
		task.Stage = model.TaskStageTraining
	} else {
		context.Logger.WithFields(
			"task-id", task.ID,
			"model", task.Model,
			"dataset", task.Dataset,
			"objective", task.Objective,
		).WriteInfo("TASK NOT IN BEGIN STAGE")
	}

	// Check the task status as it is maybe not running anymore.
	task.Status = context.getTaskStatus(task.ID)

	// Run the training stage if the task is still running.
	if task.Status == model.TaskRunning {
		if task.Stage == model.TaskStageTraining {

			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL TRAINING STARTED")

			err = context.runModelTraining(&task, modelImageName, paths, datasetPath)
			if err != nil {
				return
			}

			// Put the task in the prediction stage.
			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStage(task.ID, model.TaskStagePredicting)
			})
			task.Stage = model.TaskStagePredicting

			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL TRAINING COMPLETED")
		} else {
			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL TRAINING SKIPPED")
		}
	}

	// Check the task status as it is maybe not running anymore.
	task.Status = context.getTaskStatus(task.ID)

	// Run the predicting stage if the task is still running.
	if task.Status == model.TaskRunning {
		if task.Stage == model.TaskStagePredicting {

			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL PREDICTING STARTED")

			// Predict the training set.
			err = context.runModelPrediction(&task, modelImageName, paths, datasetPath, "train")
			if err != nil {
				return
			}

			// Predict the validation set.
			err = context.runModelPrediction(&task, modelImageName, paths, datasetPath, "val")
			if err != nil {
				return
			}

			// Put the task in the evaluation stage.
			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStage(task.ID, model.TaskStageEvaluating)
			})
			task.Stage = model.TaskStageEvaluating

			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL PREDICTING COMPLETED")
		} else {
			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL PREDICTING SKIPPED")
		}
	}

	// Check the task status as it is maybe not running anymore.
	task.Status = context.getTaskStatus(task.ID)

	// Run the evaluation stage if the task is still running.
	if task.Status == model.TaskRunning {
		if task.Stage == model.TaskStageEvaluating {

			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL EVALUATING STARTED")

			// Ensure task objective is loaded.
			objectiveImageFilePath := context.getModuleImagePath(task.Objective, model.ModuleObjective)
			objectiveImageName, err := modules.LoadImage(objectiveImageFilePath)
			if err != nil {
				err = errors.WithStack(err)
				context.Logger.WithFields(
					"module-id", task.Objective,
					"task-id", task.ID,
				).WithStack(err).WithError(err).WriteError("OBJECTIVE LOAD ERROR")

				context.repeatUntilSuccess(func() error {
					return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
				})
				return
			}

			var trainQuality, valQuality float64

			// Predict the training set.
			trainQuality, err = context.runModelEvaluationAndGetQuality(&task, objectiveImageName, paths, datasetPath, "train")
			if err != nil {
				return
			}

			// Predict the training set.
			valQuality, err = context.runModelEvaluationAndGetQuality(&task, objectiveImageName, paths, datasetPath, "val")
			if err != nil {
				return
			}

			// Update task quality.
			context.repeatUntilSuccess(func() error {
				updates := model.F{"quality": valQuality, "quality-train": trainQuality}
				_, err := context.ModelContext.UpdateTask(task.ID, updates)
				return err
			})

			// Put the task in the end stage.
			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStage(task.ID, model.TaskStageEnd)
			})
			task.Stage = model.TaskStageEnd

			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL EVALUATING COMPLETED")

			// Put the task in the end stage.
			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStage(task.ID, model.TaskStageEnd)
			})
			task.Stage = model.TaskStageEnd

		} else {
			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("MODEL EVALUATING SKIPPED")
		}
	}

	// Check the task status as it is maybe not running anymore.
	task.Status = context.getTaskStatus(task.ID)

	// Complete the task if the task is still running.
	if task.Status == model.TaskRunning {
		if task.Stage == model.TaskStageEnd {

			// Put the task in the completed state.
			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskCompleted, "")
			})
			task.Status = model.TaskCompleted

			// Log task completion.
			context.Logger.WithFields(
				"task-id", task.ID,
				"model", task.Model,
				"dataset", task.Dataset,
				"objective", task.Objective,
			).WriteInfo("TASK COMPLETED")

			// TODO: If the system fails here, we miss a chance to mark the task as completed. However, more tasks
			// might be executed and they will complete the job. Maybe this is ok.

			// Task completion could trigger job completion.
			var job model.Job
			context.repeatUntilSuccess(func() (err error) {
				job, err = context.ModelContext.GetJobByID(task.Job)
				return err
			})
			if job.MaxTasks > 0 {

				var numCompletedTasks int
				context.repeatUntilSuccess(func() (err error) {
					numCompletedTasks, err = context.ModelContext.CountTasks(model.F{"job": task.Job, "status": model.TaskCompleted})
					return err
				})

				// If the number of completed tasks is larger than the maximum number of tasks, we mark the job
				// as completed and move all remaining tasks to the terminating state.
				if uint64(numCompletedTasks) >= job.MaxTasks {
					// Mark job as completed.
					context.repeatUntilSuccess(func() error {
						_, err := context.ModelContext.UpdateJob(task.Job, model.F{"status": model.JobCompleted})
						return err
					})

					// Mark all running tasks as terminating.
					context.repeatUntilSuccess(func() error {
						return context.ModelContext.TerminateRunningTasks(task.Job)
					})

					// Log task completion.
					context.Logger.WithFields(
						"job-id", task.Job.Hex(),
						"user", task.User,
						"dataset", task.Dataset,
						"objective", task.Objective,
					).WriteInfo("JOB COMPLETED")
				}
			}
		}
	}

	// If the task is not running anymore, then we handle it.
	// This is also handled by TaskStatusMaintainerListener but we do it here for convenience.
	// TODO: Maybe remove this part.
	if task.Status != model.TaskRunning {
		if task.Status == model.TaskTerminating {
			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskTerminated, "")
			})
		} else if task.Status == model.TaskPausing {
			context.repeatUntilSuccess(func() error {
				return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskPaused, "")
			})
		}
	}

	// Unlock the task.
	context.repeatUntilSuccess(func() error {
		return context.ModelContext.UnlockTask(task.ID, context.ProcessID)
	})

}

func (context Context) getTaskStatus(id string) (status string) {
	var task model.Task
	context.repeatUntilSuccess(func() (err error) {
		task, err = context.ModelContext.GetTaskByID(id)
		return err
	})
	return task.Status
}

func (context Context) runModelTraining(task *model.Task, modelImageName string, paths storage.TaskPaths, datasetPath string) error {
	// Dump the config.
	configFilePath := filepath.Join(paths.Config, "config.json")
	ioutil.WriteFile(configFilePath, []byte(task.Config), storage.DefaultFilePerm)

	// Run the training.
	trainDatasetPath := filepath.Join(datasetPath, "train")
	command := []string{
		"train",
		"--data", modules.MntPrefix + trainDatasetPath,
		"--conf", modules.MntPrefix + configFilePath,
		"--output", modules.MntPrefix + paths.Parameters,
	}
	outReader, err := modules.RunContainerAndCollectOutput(modelImageName, nil, command)
	if err != nil {
		err = errors.WithStack(err)
		context.Logger.WithFields(
			"module-id", task.Model,
			"task-id", task.ID,
		).WithStack(err).WithError(err).WriteError("MODEL CONTAINER START ERROR")

		context.repeatUntilSuccess(func() error {
			return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
		})
		return err
	}
	defer outReader.Close()

	// Write the output to the train log.
	trainLogData, err := ioutil.ReadAll(outReader)
	if err != nil {
		err = errors.WithStack(err)
		context.Logger.WithFields(
			"module-id", task.Model,
			"task-id", task.ID,
		).WithStack(err).WithError(err).WriteError("MODEL CONTAINER OUTPUT READ ERROR")

		context.repeatUntilSuccess(func() error {
			return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
		})
		return err
	}
	ioutil.WriteFile(filepath.Join(paths.Logs, "train.log"), trainLogData, storage.DefaultFilePerm)

	return nil
}

func (context Context) runModelPrediction(task *model.Task, modelImageName string, paths storage.TaskPaths, datasetPath string, subdir string) error {

	// Run the prediction.
	valDatasetPath := filepath.Join(datasetPath, subdir)
	valOutputPath := filepath.Join(paths.Predictions, subdir)
	command := []string{
		"predict",
		"--data", modules.MntPrefix + valDatasetPath,
		"--memory", modules.MntPrefix + paths.Parameters,
		"--output", modules.MntPrefix + valOutputPath,
	}
	outReader, err := modules.RunContainerAndCollectOutput(modelImageName, nil, command)
	if err != nil {
		err = errors.WithStack(err)
		context.Logger.WithFields(
			"module-id", task.Model,
			"task-id", task.ID,
		).WithStack(err).WithError(err).WriteError("MODEL CONTAINER START ERROR")

		context.repeatUntilSuccess(func() error {
			return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
		})
		return err
	}
	defer outReader.Close()

	// Write the output to the predict log.
	predictLogData, err := ioutil.ReadAll(outReader)
	if err != nil {
		err = errors.WithStack(err)
		context.Logger.WithFields(
			"module-id", task.Model,
			"task-id", task.ID,
		).WithStack(err).WithError(err).WriteError("MODEL CONTAINER OUTPUT READ ERROR")

		context.repeatUntilSuccess(func() error {
			return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
		})
		return err
	}
	ioutil.WriteFile(filepath.Join(paths.Logs, "predict."+subdir+".log"), predictLogData, storage.DefaultFilePerm)

	return nil
}

func (context Context) runModelEvaluationAndGetQuality(task *model.Task, objectiveImageName string, paths storage.TaskPaths, datasetPath string, subdir string) (float64, error) {

	// Run the evaluation.
	valDatasetPath := filepath.Join(datasetPath, subdir)
	valOutputPath := filepath.Join(paths.Predictions, subdir)
	command := []string{
		"eval",
		"--actual", modules.MntPrefix + valDatasetPath,
		"--predicted", modules.MntPrefix + valOutputPath,
	}
	outReader, err := modules.RunContainerAndCollectOutput(objectiveImageName, nil, command)
	if err != nil {
		err = errors.WithStack(err)
		context.Logger.WithFields(
			"module-id", task.Objective,
			"task-id", task.ID,
		).WithStack(err).WithError(err).WriteError("OBJECTIVE CONTAINER START ERROR")

		context.repeatUntilSuccess(func() error {
			return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
		})
		return 0, err
	}
	defer outReader.Close()

	// Parse evaluations.
	scanner := bufio.NewScanner(outReader)
	lines := []string{}
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	// The last line should contain the quality.
	qualityString := strings.TrimSpace(lines[len(lines)-1])
	quality, err := strconv.ParseFloat(qualityString, 64)
	if err != nil {
		err = errors.WithStack(err)
		context.Logger.WithFields(
			"module-id", task.Objective,
			"task-id", task.ID,
		).WithStack(err).WithError(err).WriteError("OBJECTIVE QUALITY PARSE ERROR")

		context.repeatUntilSuccess(func() error {
			return context.ModelContext.UpdateTaskStatus(task.ID, model.TaskError, err.Error())
		})
		return 0, err
	}

	// Dump evaluation lines.
	lines = lines[:len(lines)-1]
	evalFileName := filepath.Join(paths.Evaluations, "evals."+subdir+".log")
	evalLines := strings.Join(lines, "\n")
	err = ioutil.WriteFile(evalFileName, []byte(evalLines), storage.DefaultFilePerm)
	if err != nil {
		panic(err)
	}

	return quality, nil
}
