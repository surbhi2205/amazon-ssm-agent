package outofproc

import (
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/docmanager/model"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/basicexecuter"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/outofproc/channel"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/outofproc/messaging"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/outofproc/proc"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/task"
)

type Backend messaging.MessagingBackend

//see differences between zombie and orphan: https://www.gmarik.info/blog/2012/orphan-vs-zombie-vs-daemon-processes/
const (
	//TODO prolong this value once we go to production
	defaultZombieProcessTimeout = 2 * time.Second
	//command maximum timeout
	defaultOrphanProcessTimeout = 172800 * time.Second
)

type OutOfProcExecuter struct {
	basicexecuter.BasicExecuter
	docState   *model.DocumentState
	ctx        context.T
	cancelFlag task.CancelFlag
}

var channelCreator = func(log log.T, mode channel.Mode, documentID string) (channel.Channel, error, bool) {
	return channel.CreateFileChannel(log, mode, documentID)
}

var processFinder = func(log log.T, procinfo model.OSProcInfo) bool {
	//If ProcInfo is not initailized
	//pid 0 is reserved for kernel on both linux and windows, so the assumption is safe here
	if procinfo.Pid == 0 {
		return false
	}

	return proc.IsProcessExists(log, procinfo.Pid, procinfo.StartTime)
}

var processCreator = func(name string, argv []string) (proc.OSProcess, error) {
	return proc.StartProcess(name, argv)
}

func NewOutOfProcExecuter(ctx context.T) *OutOfProcExecuter {
	return &OutOfProcExecuter{
		BasicExecuter: *basicexecuter.NewBasicExecuter(ctx),
		ctx:           ctx.With("[OutOfProcExecuter]"),
	}
}

//Run() prepare the ipc channel, create a data processing backend and start messaging with docment worker
func (e *OutOfProcExecuter) Run(
	cancelFlag task.CancelFlag,
	docStore executer.DocumentStore) chan contracts.DocumentResult {
	docState := docStore.Load()
	e.docState = &docState
	e.cancelFlag = cancelFlag
	documentID := docState.DocumentInformation.DocumentID

	//update context with the document id
	e.ctx = e.ctx.With("[" + documentID + "]")
	log := e.ctx.Log()

	//stopTimer signals messaging routine to stop, it's buffered because it needs to exit if messaging is already stopped and not receiving anymore
	stopTimer := make(chan bool, 1)
	//start prepare messaging
	//if anything fails during the prep stage, use in-proc Runner
	ipc, err := e.initialize(stopTimer)
	if err != nil {
		log.Errorf("failed to prepare outofproc executer, falling back to InProc Executer")
		return e.BasicExecuter.Run(cancelFlag, docStore)
	} else {
		//create reply channel
		resChan := make(chan contracts.DocumentResult, len(e.docState.InstancePluginsInformation)+1)
		//launch the messaging go-routine
		go func(store executer.DocumentStore) {
			defer func() {
				if msg := recover(); msg != nil {
					log.Errorf("Executer go-routine panic: %v", msg)
				}
				//save the overall result and signal called that Executer is done
				store.Save(*e.docState)
				log.Info("Executer closed")
				close(resChan)
			}()
			e.messaging(log, ipc, resChan, cancelFlag, stopTimer)
		}(docStore)

		return resChan
	}
}

//Executer spins up an ipc transmission worker, it creates a Data processing backend and hands off the backend to the ipc worker
//ipc worker and data backend act as 2 threads exchange raw json messages, and messaging protocol happened in data backend, data backend is self-contained and exit when command finishes accordingly
//Executer however does hold a timer to the worker to forcefully termniate both of them
func (e *OutOfProcExecuter) messaging(log log.T, ipc channel.Channel, resChan chan contracts.DocumentResult, cancelFlag task.CancelFlag, stopTimer chan bool) {

	//handoff reply functionalities to data backend.
	backend := messaging.NewExecuterBackend(resChan, e.docState, cancelFlag)
	//handoff the data backend to messaging worker
	if err := messaging.Messaging(log, ipc, backend, stopTimer); err != nil {
		//the messaging worker encountered error, either ipc run into error or data backend throws error
		log.Errorf("messaging worker encountered error: %v", err)
		if e.docState.DocumentInformation.DocumentStatus == contracts.ResultStatusInProgress ||
			e.docState.DocumentInformation.DocumentStatus == "" ||
			e.docState.DocumentInformation.DocumentStatus == contracts.ResultStatusNotStarted {
			e.docState.DocumentInformation.DocumentStatus = contracts.ResultStatusFailed
			log.Info("document failed half way, sending fail message...")
			resChan <- e.generateUnexpectedFailResult()
		}
		//destroy the channel
		ipc.Destroy()
	}
}

func (e *OutOfProcExecuter) generateUnexpectedFailResult() contracts.DocumentResult {
	var docResult contracts.DocumentResult
	docResult.MessageID = e.docState.DocumentInformation.MessageID
	docResult.AssociationID = e.docState.DocumentInformation.AssociationID
	docResult.DocumentName = e.docState.DocumentInformation.DocumentName
	docResult.NPlugins = len(e.docState.InstancePluginsInformation)
	docResult.DocumentVersion = e.docState.DocumentInformation.DocumentVersion
	docResult.Status = contracts.ResultStatusFailed
	docResult.PluginResults = make(map[string]*contracts.PluginResult)
	res := e.docState.InstancePluginsInformation[0].Result
	res.Output = "out of process execution failed unexpectedly"
	res.Status = contracts.ResultStatusFailed
	docResult.PluginResults[e.docState.InstancePluginsInformation[0].Id] = &res
	return docResult
}

//prepare the channel for messaging as well as launching the document worker process, if the channel already exists, re-open it.
//launch timeout timer based off the discovered process status
func (e *OutOfProcExecuter) initialize(stopTimer chan bool) (ipc channel.Channel, err error) {
	log := e.ctx.Log()
	var found bool
	documentID := e.docState.DocumentInformation.DocumentID
	ipc, err, found = channelCreator(log, channel.ModeMaster, documentID)

	if err != nil {
		log.Errorf("failed to create ipc channel: %v", err)
		return
	}
	if found {
		log.Info("discovered old channel object, trying to find detached process...")
		var stopTime time.Duration
		procInfo := e.docState.DocumentInformation.ProcInfo
		if processFinder(log, procInfo) {
			log.Infof("found orphan process: %v, start time: %v", procInfo.Pid, procInfo.StartTime)
			stopTime = defaultOrphanProcessTimeout
		} else {
			log.Infof("process: %v not found, treat as exited", procInfo.Pid)
			stopTime = defaultZombieProcessTimeout
		}
		go timeout(stopTimer, stopTime, e.cancelFlag)
	} else {
		log.Debug("channel not found, starting a new process...")
		var process proc.OSProcess
		if process, err = processCreator(appconfig.DefaultDocumentWorker, proc.FormArgv(documentID)); err != nil {
			log.Errorf("start process: %v error: %v", appconfig.DefaultDocumentWorker, err)
			//make sure close the channel
			ipc.Destroy()
			return
		} else {
			log.Debugf("successfully launched new process: %v", process.Pid())
		}
		e.docState.DocumentInformation.ProcInfo = model.OSProcInfo{
			Pid:       process.Pid(),
			StartTime: process.StartTime(),
		}
		//TODO add command timeout as well, in case process get stuck
		go e.WaitForProcess(stopTimer, process)

	}

	return
}

func (e *OutOfProcExecuter) WaitForProcess(stopTimer chan bool, process proc.OSProcess) {
	log := e.ctx.Log()
	waitReturned := false
	go func() {
		//if job complete but process still hangs, kill it.
		e.cancelFlag.Wait()
		if !waitReturned && e.cancelFlag.State() == task.Completed {
			process.Kill()
		}
	}()
	if err := process.Wait(); err != nil {
		log.Errorf("process: %v exits unsuccessfully, error message: %v", process.Pid(), err)
	}
	waitReturned = true
	timeout(stopTimer, defaultZombieProcessTimeout, e.cancelFlag)
}

func timeout(stopTimer chan bool, duration time.Duration, cancelFlag task.CancelFlag) {
	stopChan := make(chan bool)
	//TODO refactor cancelFlag.Wait() to return channel instead of blocking call
	go func() {
		cancelFlag.Wait()
		stopChan <- true
	}()
	select {
	case <-time.After(duration):
		stopTimer <- true
	case <-stopChan:
	}

}