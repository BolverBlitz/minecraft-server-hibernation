package progmgr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"msh/lib/config"
	"msh/lib/errco"
	"msh/lib/model"
	"msh/lib/servctrl"
	"msh/lib/servstats"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
)

var (
	// msh version
	MshVersion string = "v2.4.5"

	// CheckedUpdateC communicates to main func that the first update check
	// has been done and msh can continue
	CheckedUpdateC chan bool = make(chan bool, 1)

	// api protocol version
	protv int = 2

	// msh program
	msh *program = &program{
		startTime: time.Now(),
		sigExit:   make(chan os.Signal, 1),
	}
)

type program struct {
	startTime time.Time      // msh program start time
	sigExit   chan os.Signal // channel through which OS termination signals are notified
}

// MshMgr handles exit signal and updates for msh
// [goroutine]
func MshMgr() {
	// set sigExit to relay termination signals
	signal.Notify(msh.sigExit, syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT)

	// initialize sgm variables
	sgm.reset(0) // segment duration initialized to 0 so that update check can be executed immediately

	// start segment manager
	go sgm.sgmMgr()

	for {
	mainselect:
		select {
		// msh termination signal is received
		case <-msh.sigExit:
			// stop the minecraft server with no player check
			errMsh := servctrl.StopMS(false)
			if errMsh != nil {
				errco.LogMshErr(errMsh.AddTrace("MshMgr"))
			}

			// send last statistics before exiting
			go checkUpdReq(true)

			// wait 1 second to let the server go into stopping mode
			time.Sleep(time.Second)

			switch servstats.Stats.Status {
			case errco.SERVER_STATUS_STOPPING:
				// if server is correctly stopping, wait for minecraft server to exit
				errco.Logln(errco.LVL_D, "MshMgr: waiting for minecraft server terminal to exit (server is stopping)")
				servctrl.ServTerm.Wg.Wait()

			case errco.SERVER_STATUS_OFFLINE:
				// if server is offline, then it's safe to continue
				errco.Logln(errco.LVL_D, "MshMgr: minecraft server terminal already exited (server is offline)")

			default:
				errco.Logln(errco.LVL_D, "MshMgr: stop command does not seem to be stopping server during forceful shutdown")
			}

			// exit
			errco.Logln(errco.LVL_A, "exiting msh")
			os.Exit(0)

		// check for update when segment ends
		case <-sgm.end.C:
			// send check update request
			errco.Logln(errco.LVL_D, "sending check update request...")
			res, errMsh := checkUpdReq(false)
			if errMsh != nil {
				errco.LogMshErr(errMsh.AddTrace("UpdateManager"))
				sgm.prolong(10 * time.Minute)
				break mainselect
			}

			// check check update response status code
			switch res.StatusCode {
			case 200:
				errco.Logln(errco.LVL_D, "resetting segment...")
				sgm.reset(sgm.getRatelimit(res))
			case 403:
				errco.LogMshErr(errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "MshMgr", "client is unauthorized"))
				os.Exit(1)
			default:
				errco.LogMshErr(errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "MshMgr", "response status code is "+res.Status))
				errco.Logln(errco.LVL_D, "prolonging segment...")
				sgm.prolong(sgm.getRatelimit(res))
				break mainselect
			}

			// analyze check update response
			errco.Logln(errco.LVL_D, "analyzing check update response...")
			resJson, errMsh := checkUpdAnalyze(res)
			if errMsh != nil {
				errco.LogMshErr(errMsh.AddTrace("UpdateManager"))
				break mainselect
			}

			// log res message
			for _, m := range resJson.Messages {
				errco.Logln(errco.LVL_B, m)
			}

			// check version result
			switch resJson.Result {
			case "dep": // local version deprecated
				// don't check if NotifyUpdate is set to true
				// override ConfigRuntime variables to display deprecated error message
				config.ConfigRuntime.Msh.InfoHibernation = "                   §fserver status:\n                   §b§lHIBERNATING\n                   §b§cmsh version DEPRECATED"
				config.ConfigRuntime.Msh.InfoStarting = "                   §fserver status:\n                    §6§lWARMING UP\n                   §b§cmsh version DEPRECATED"
				config.ConfigRuntime.Msh.NotifyUpdate = true

				notification := fmt.Sprintf("msh (%s) is deprecated: please update msh to %s!", MshVersion, resJson.Official.Version)

				// write in console log
				errco.Logln(errco.LVL_A, notification)

				// set push notification message
				sgm.push.message = notification
			case "upd": // local version to update
				if config.ConfigRuntime.Msh.NotifyUpdate {
					notification := fmt.Sprintf("msh (%s) is now available: visit github to update!", resJson.Official.Version)

					// write in console log
					errco.Logln(errco.LVL_A, notification)

					// set push notification message
					sgm.push.message = notification
				}
			case "ok": // local version is ok
				if config.ConfigRuntime.Msh.NotifyUpdate {
					// write in console log
					errco.Logln(errco.LVL_A, "msh (%s) is updated", MshVersion)
				}
			case "dev": // local version is a developement version
				if config.ConfigRuntime.Msh.NotifyUpdate {
					// write in console log
					errco.Logln(errco.LVL_A, "msh (%s) is running a dev release", MshVersion)
				}
			case "uno": // local version is unofficial
				if config.ConfigRuntime.Msh.NotifyUpdate {
					// write in console log
					errco.Logln(errco.LVL_A, "msh (%s) is running an unofficial release", MshVersion)
				}
			default: // an error occurred
				errco.LogMshErr(errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "checkUpdAnalyze", "invalid result from server"))
				break mainselect
			}
		}
	}
}

// checkUpdReq logs segment stats and checks for updates.
func checkUpdReq(preTerm bool) (*http.Response, *errco.Error) {
	// before returning, communicate that update check is done
	defer func() {
		select {
		case CheckedUpdateC <- true:
		default:
		}
	}()

	// build request struct
	reqJson := buildReq(preTerm)

	// marshal request struct
	reqByte, err := json.Marshal(reqJson)
	if err != nil {
		return nil, errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "checkUpdReq", err.Error())
	}

	// build http request
	url := fmt.Sprintf("http://msh.gekware.net/api/v%d", protv)
	req, err := http.NewRequest("GET", url, bytes.NewReader(reqByte))
	if err != nil {
		return nil, errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "checkUpdReq", err.Error())
	}

	// add header User-Agent: msh/<msh-version> (<system-information>) <platform> (<platform-details>) <extensions>
	req.Header.Add("User-Agent", fmt.Sprintf("msh/%s (%s) %s", MshVersion, runtime.GOOS, runtime.GOARCH))

	errco.Logln(errco.LVL_E, "%smsh --> mshc%s:%v", errco.COLOR_PURPLE, errco.COLOR_RESET, reqByte)

	// execute http request
	client := &http.Client{Timeout: 4 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "checkUpdReq", err.Error())
	}

	return res, nil
}

// checkUpdAnalyze analyzes server response
func checkUpdAnalyze(res *http.Response) (*model.Api2Res, *errco.Error) {
	defer res.Body.Close()

	// read http response
	resByte, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "checkUpdAnalyze", err.Error())
	}

	errco.Logln(errco.LVL_E, "%smshc --> msh%s:%v", errco.COLOR_PURPLE, errco.COLOR_RESET, resByte)

	// load res data into resJson
	var resJson *model.Api2Res
	err = json.Unmarshal(resByte, &resJson)
	if err != nil {
		return nil, errco.NewErr(errco.ERROR_VERSION, errco.LVL_D, "checkUpdAnalyze", err.Error())
	}

	return resJson, nil
}

// buildReq builds Api2Req
func buildReq(preTerm bool) *model.Api2Req {
	reqJson := &model.Api2Req{}

	reqJson.Protv = protv

	reqJson.Msh.Mshv = MshVersion
	reqJson.Msh.ID = config.ConfigRuntime.Msh.ID
	reqJson.Msh.Uptime = int(time.Since(msh.startTime).Seconds())
	reqJson.Msh.AllowSuspend = config.ConfigRuntime.Msh.AllowSuspend
	reqJson.Msh.Sgm.Seconds = sgm.stats.seconds
	reqJson.Msh.Sgm.SecondsHibe = sgm.stats.secondsHibe
	reqJson.Msh.Sgm.CpuUsage = sgm.stats.cpuUsage
	reqJson.Msh.Sgm.MemUsage = sgm.stats.memUsage
	reqJson.Msh.Sgm.PlayerSec = sgm.stats.playerSec
	reqJson.Msh.Sgm.PreTerm = preTerm

	reqJson.Machine.Os = runtime.GOOS
	reqJson.Machine.Platform = runtime.GOARCH
	reqJson.Machine.Javav = config.Javav
	reqJson.Machine.Stats.CoresMsh = runtime.NumCPU()
	if cores, err := cpu.Counts(true); err != nil {
		errco.LogMshErr(errco.NewErr(errco.ERROR_GET_CORES, errco.LVL_D, "buildReq", err.Error())) // non blocking error
		reqJson.Machine.Stats.Cores = -1
	} else {
		reqJson.Machine.Stats.Cores = cores
	}
	if memInfo, err := mem.VirtualMemory(); err != nil {
		errco.LogMshErr(errco.NewErr(errco.ERROR_GET_MEMORY, errco.LVL_D, "buildReq", err.Error())) // non blocking error
		reqJson.Machine.Stats.Mem = -1
	} else {
		reqJson.Machine.Stats.Mem = int(memInfo.Total)
	}

	reqJson.Server.Uptime = servctrl.TermUpTime()
	reqJson.Server.Minev = config.ConfigRuntime.Server.Version
	reqJson.Server.MineProt = config.ConfigRuntime.Server.Protocol

	return reqJson
}
