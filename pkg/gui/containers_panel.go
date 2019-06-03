package gui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/fatih/color"
	"github.com/go-errors/errors"
	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazydocker/pkg/commands"
	"github.com/jesseduffield/lazydocker/pkg/utils"
	"golang.org/x/xerrors"
)

// list panel functions

func (gui *Gui) getContainerContexts() []string {
	return []string{"logs", "config", "stats"}
}

func (gui *Gui) getSelectedContainer(g *gocui.Gui) (*commands.Container, error) {
	selectedLine := gui.State.Panels.Containers.SelectedLine
	if selectedLine == -1 {
		return &commands.Container{}, gui.Errors.ErrNoContainers
	}

	return gui.DockerCommand.Containers[selectedLine], nil
}

func (gui *Gui) handleContainersFocus(g *gocui.Gui, v *gocui.View) error {
	if gui.popupPanelFocused() {
		return nil
	}

	cx, cy := v.Cursor()
	_, oy := v.Origin()

	prevSelectedLine := gui.State.Panels.Containers.SelectedLine
	newSelectedLine := cy - oy

	if newSelectedLine > len(gui.DockerCommand.Containers)-1 || len(utils.Decolorise(gui.DockerCommand.Containers[newSelectedLine].Name)) < cx {
		return gui.handleContainerSelect(gui.g, v)
	}

	gui.State.Panels.Containers.SelectedLine = newSelectedLine

	if prevSelectedLine == newSelectedLine && gui.currentViewName() == v.Name() {
		return nil
	}

	return gui.handleContainerSelect(gui.g, v)
}

func (gui *Gui) handleContainerSelect(g *gocui.Gui, v *gocui.View) error {
	if _, err := gui.g.SetCurrentView(v.Name()); err != nil {
		return err
	}

	container, err := gui.getSelectedContainer(g)
	if err != nil {
		if err != gui.Errors.ErrNoContainers {
			return err
		}
		return nil
	}

	key := container.ID + "-" + gui.getContainerContexts()[gui.State.Panels.Containers.ContextIndex]
	if gui.State.Panels.Main.ObjectKey == key {
		return nil
	} else {
		gui.State.Panels.Main.ObjectKey = key
	}

	if err := gui.focusPoint(0, gui.State.Panels.Containers.SelectedLine, len(gui.DockerCommand.Containers), v); err != nil {
		return err
	}

	mainView := gui.getMainView()

	mainView.Clear()
	mainView.SetOrigin(0, 0)
	mainView.SetCursor(0, 0)

	switch gui.getContainerContexts()[gui.State.Panels.Containers.ContextIndex] {
	case "logs":
		if err := gui.renderContainerLogs(mainView, container); err != nil {
			return err
		}
	case "config":
		if err := gui.renderContainerConfig(mainView, container); err != nil {
			return err
		}
	case "stats":
		if err := gui.renderContainerStats(mainView, container); err != nil {
			return err
		}
	default:
		return errors.New("Unknown context for containers panel")
	}

	return nil
}

func (gui *Gui) renderContainerConfig(mainView *gocui.View, container *commands.Container) error {
	mainView.Autoscroll = false
	mainView.Title = "Config"

	data, err := json.MarshalIndent(&container.Details, "", "  ")
	if err != nil {
		return err
	}

	return gui.T.NewTask(func(stop chan struct{}) {
		gui.renderString(gui.g, "main", string(data))
	})
}

func (gui *Gui) renderContainerStats(mainView *gocui.View, container *commands.Container) error {
	mainView.Autoscroll = false
	mainView.Title = "Stats"
	mainView.Wrap = false

	return gui.T.NewTickerTask(time.Second, func() {
		width, _ := mainView.Size()

		contents, err := container.RenderStats(width)
		if err != nil {
			gui.createErrorPanel(gui.g, err.Error())
		}

		gui.reRenderString(gui.g, "main", contents)
	})
}

func (gui *Gui) renderContainerLogs(mainView *gocui.View, container *commands.Container) error {
	mainView.Autoscroll = true
	mainView.Title = "Logs"

	if container.Details.Config.OpenStdin {
		return gui.renderLogsForTTYContainer(mainView, container)
	}
	return gui.renderLogsForRegularContainer(mainView, container)
}

func (gui *Gui) renderLogsForRegularContainer(mainView *gocui.View, container *commands.Container) error {
	var cmd *exec.Cmd
	cmd = gui.OSCommand.RunCustomCommand("docker logs --since=60m --timestamps --follow " + container.ID)

	cmd.Stdout = mainView
	cmd.Stderr = mainView

	go gui.runProcessWithLock(cmd)

	return nil
}

func (gui *Gui) obtainLock() {
	gui.State.MainProcessChan <- struct{}{}
	gui.State.MainProcessMutex.Lock()
}

func (gui *Gui) releaseLock() {
	gui.State.MainProcessMutex.Unlock()
}

func (gui *Gui) runProcessWithLock(cmd *exec.Cmd) {
	gui.T.NewTask(func(stop chan struct{}) {
		cmd.Start()

		go func() {
			<-stop
			cmd.Process.Kill()
		}()

		cmd.Wait()
	})
}

func (gui *Gui) renderLogsForTTYContainer(mainView *gocui.View, container *commands.Container) error {
	var cmd *exec.Cmd
	cmd = gui.OSCommand.RunCustomCommand("docker logs --since=60m --follow " + container.ID)

	r, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	go func() {
		s := bufio.NewScanner(r)
		s.Split(bufio.ScanLines)
		for s.Scan() {
			// I might put a check on the stopped channel here. Would mean more code duplication though
			mainView.Write(append(s.Bytes(), '\n'))
		}
	}()

	go gui.runProcessWithLock(cmd)
	return nil
}

func (gui *Gui) refreshContainersAndServices() error {
	containersView := gui.getContainersView()
	if containersView == nil {
		// if the containersView hasn't been instantiated yet we just return
		return nil
	}

	// keep track of current service selected so that we can reposition our cursor if it moves position in the list
	sl := gui.State.Panels.Services.SelectedLine
	var selectedService *commands.Service
	if len(gui.DockerCommand.Services) > 0 {
		selectedService = gui.DockerCommand.Services[sl]
	}

	if err := gui.refreshStateContainersAndServices(); err != nil {
		return err
	}

	// see if our selected service has moved
	if selectedService != nil {
		for i, service := range gui.DockerCommand.Services {
			if service.ID == selectedService.ID {
				if i == sl {
					break
				}
				gui.State.Panels.Services.SelectedLine = i
				if err := gui.focusPoint(0, i, len(gui.DockerCommand.Services), gui.getServicesView()); err != nil {
					return err
				}
			}
		}
	}

	if len(gui.DockerCommand.Containers) > 0 && gui.State.Panels.Containers.SelectedLine == -1 {
		gui.State.Panels.Containers.SelectedLine = 0
	}
	if len(gui.DockerCommand.Containers)-1 < gui.State.Panels.Containers.SelectedLine {
		gui.State.Panels.Containers.SelectedLine = len(gui.DockerCommand.Containers) - 1
	}

	// doing the exact same thing for services
	if len(gui.DockerCommand.Services) > 0 && gui.State.Panels.Services.SelectedLine == -1 {
		gui.State.Panels.Services.SelectedLine = 0
	}
	if len(gui.DockerCommand.Services)-1 < gui.State.Panels.Services.SelectedLine {
		gui.State.Panels.Services.SelectedLine = len(gui.DockerCommand.Services) - 1
	}

	gui.g.Update(func(g *gocui.Gui) error {
		containersView.Clear()
		isFocused := gui.g.CurrentView().Name() == "containers"
		list, err := utils.RenderList(gui.DockerCommand.Containers, utils.IsFocused(isFocused))
		if err != nil {
			return err
		}
		fmt.Fprint(containersView, list)

		if containersView == g.CurrentView() {
			if err := gui.handleContainerSelect(g, containersView); err != nil {
				return err
			}
		}

		// doing the exact same thing for services
		if !gui.DockerCommand.InDockerComposeProject {
			return nil
		}
		servicesView := gui.getServicesView()
		servicesView.Clear()
		isFocused = gui.g.CurrentView().Name() == "services"
		list, err = utils.RenderList(gui.DockerCommand.Services, utils.IsFocused(isFocused))
		if err != nil {
			return err
		}
		fmt.Fprint(servicesView, list)

		if servicesView == g.CurrentView() {
			return gui.handleServiceSelect(g, servicesView)
		}
		return nil
	})

	return nil
}

func (gui *Gui) refreshStateContainersAndServices() error {
	return gui.DockerCommand.GetContainersAndServices()
}

func (gui *Gui) handleContainersNextLine(g *gocui.Gui, v *gocui.View) error {
	if gui.popupPanelFocused() {
		return nil
	}

	panelState := gui.State.Panels.Containers
	gui.changeSelectedLine(&panelState.SelectedLine, len(gui.DockerCommand.Containers), false)

	return gui.handleContainerSelect(gui.g, v)
}

func (gui *Gui) handleContainersPrevLine(g *gocui.Gui, v *gocui.View) error {
	if gui.popupPanelFocused() {
		return nil
	}

	panelState := gui.State.Panels.Containers
	gui.changeSelectedLine(&panelState.SelectedLine, len(gui.DockerCommand.Containers), true)

	return gui.handleContainerSelect(gui.g, v)
}

func (gui *Gui) handleContainersPrevContext(g *gocui.Gui, v *gocui.View) error {
	contexts := gui.getContainerContexts()
	if gui.State.Panels.Containers.ContextIndex >= len(contexts)-1 {
		gui.State.Panels.Containers.ContextIndex = 0
	} else {
		gui.State.Panels.Containers.ContextIndex++
	}

	gui.handleContainerSelect(gui.g, v)

	return nil
}

func (gui *Gui) handleContainersNextContext(g *gocui.Gui, v *gocui.View) error {
	contexts := gui.getContainerContexts()
	if gui.State.Panels.Containers.ContextIndex <= 0 {
		gui.State.Panels.Containers.ContextIndex = len(contexts) - 1
	} else {
		gui.State.Panels.Containers.ContextIndex--
	}

	gui.handleContainerSelect(gui.g, v)

	return nil
}

type removeContainerOption struct {
	description   string
	command       string
	configOptions types.ContainerRemoveOptions
	runCommand    bool
}

// GetDisplayStrings is a function.
func (r *removeContainerOption) GetDisplayStrings(isFocused bool) []string {
	return []string{r.description, color.New(color.FgRed).Sprint(r.command)}
}

func (gui *Gui) handleContainersRemoveMenu(g *gocui.Gui, v *gocui.View) error {
	container, err := gui.getSelectedContainer(g)
	if err != nil {
		return nil
	}

	options := []*removeContainerOption{
		{
			description:   gui.Tr.SLocalize("remove"),
			command:       "docker rm " + container.ID[1:10],
			configOptions: types.ContainerRemoveOptions{},
		},
		{
			description:   gui.Tr.SLocalize("removeWithVolumes"),
			command:       "docker rm --volumes " + container.ID[1:10],
			configOptions: types.ContainerRemoveOptions{RemoveVolumes: true},
		},
		{
			description: gui.Tr.SLocalize("cancel"),
		},
	}

	handleMenuPress := func(index int) error {
		if options[index].command == "" {
			return nil
		}
		configOptions := options[index].configOptions

		return gui.WithWaitingStatus(gui.Tr.SLocalize("RemovingStatus"), func() error {
			if cerr := container.Remove(configOptions); cerr != nil {
				var originalErr commands.ComplexError
				if xerrors.As(cerr, &originalErr) {
					if originalErr.Code == commands.MustStopContainer {
						return gui.createConfirmationPanel(gui.g, v, gui.Tr.SLocalize("Confirm"), gui.Tr.SLocalize("mustForceToRemoveContainer"), func(g *gocui.Gui, v *gocui.View) error {
							return gui.WithWaitingStatus(gui.Tr.SLocalize("RemovingStatus"), func() error {
								configOptions.Force = true
								if err := container.Remove(configOptions); err != nil {
									return err
								}
								return gui.refreshContainersAndServices()
							})
						}, nil)
					}
				} else {
					return gui.createErrorPanel(gui.g, cerr.Error())
				}
			}

			return gui.refreshContainersAndServices()
		})

	}

	return gui.createMenu("", options, len(options), handleMenuPress)
}

func (gui *Gui) handleContainerStop(g *gocui.Gui, v *gocui.View) error {
	container, err := gui.getSelectedContainer(g)
	if err != nil {
		return nil
	}

	return gui.createConfirmationPanel(gui.g, v, gui.Tr.SLocalize("Confirm"), gui.Tr.SLocalize("StopContainer"), func(g *gocui.Gui, v *gocui.View) error {
		return gui.WithWaitingStatus(gui.Tr.SLocalize("StoppingStatus"), func() error {
			if err := container.Stop(); err != nil {
				return gui.createErrorPanel(gui.g, err.Error())
			}

			return gui.refreshContainersAndServices()
		})

	}, nil)
}

func (gui *Gui) handleContainerRestart(g *gocui.Gui, v *gocui.View) error {
	container, err := gui.getSelectedContainer(g)
	if err != nil {
		return nil
	}

	return gui.WithWaitingStatus(gui.Tr.SLocalize("RestartingStatus"), func() error {
		if err := container.Restart(); err != nil {
			return gui.createErrorPanel(gui.g, err.Error())
		}

		return gui.refreshContainersAndServices()
	})
}

func (gui *Gui) handleContainerAttach(g *gocui.Gui, v *gocui.View) error {
	container, err := gui.getSelectedContainer(g)
	if err != nil {
		return nil
	}

	c, err := container.Attach()
	if err != nil {
		return gui.createErrorPanel(gui.g, err.Error())
	}

	gui.SubProcess = c
	return gui.Errors.ErrSubProcess
}
