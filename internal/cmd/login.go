// Package cmd provides command-line interface functionality for the CLI Proxy API server.
// It includes authentication flows for various AI service providers, service startup,
// and other command-line operations.
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/gemini"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// DoLogin handles Google Gemini authentication using the shared authentication manager.
// It initiates the OAuth flow for Google Gemini services, performs the legacy CLI user setup,
// and saves the authentication tokens to the configured auth directory.
//
// Parameters:
//   - cfg: The application configuration
//   - projectID: Optional Google Cloud project ID for Gemini services
//   - options: Login options including browser behavior and prompts
func DoLogin(cfg *config.Config, projectID string, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	ctx := context.Background()

	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}

	trimmedProjectID := strings.TrimSpace(projectID)
	callbackPrompt := promptFn
	if trimmedProjectID == "" {
		callbackPrompt = nil
	}

	loginOpts := &sdkAuth.LoginOptions{
		NoBrowser:    options.NoBrowser,
		ProjectID:    trimmedProjectID,
		CallbackPort: options.CallbackPort,
		Metadata:     map[string]string{},
		Prompt:       callbackPrompt,
	}

	authenticator := sdkAuth.NewGeminiAuthenticator()
	record, errLogin := authenticator.Login(ctx, cfg, loginOpts)
	if errLogin != nil {
		log.Errorf("Gemini authentication failed: %v", errLogin)
		return
	}

	storage, okStorage := record.Storage.(*gemini.TokenStorage)
	if !okStorage || storage == nil {
		log.Error("Gemini authentication failed: unsupported token storage")
		return
	}

	geminiAuth := gemini.NewAuth()
	httpClient, errClient := geminiAuth.GetAuthenticatedClient(
		ctx, storage, cfg, &gemini.WebLoginOptions{
			NoBrowser:    options.NoBrowser,
			CallbackPort: options.CallbackPort,
			Prompt:       callbackPrompt,
		},
	)
	if errClient != nil {
		log.Errorf("Gemini authentication failed: %v", errClient)
		return
	}

	log.Info("Authentication successful.")

	var activatedProjects []string

	useGoogleOne := false
	if trimmedProjectID == "" && promptFn != nil {
		fmt.Println("\nSelect login mode:")
		fmt.Println("  1. Code Assist  (GCP project, manual selection)")
		fmt.Println("  2. Google One   (personal account, auto-discover project)")
		choice, errPrompt := promptFn("Enter choice [1/2] (default: 1): ")
		if errPrompt == nil && strings.TrimSpace(choice) == "2" {
			useGoogleOne = true
		}
	}

	if useGoogleOne {
		log.Info("Google One mode: auto-discovering project...")
		if errSetup := gemini.PerformCLISetup(ctx, httpClient, storage, "", nil); errSetup != nil {
			log.Errorf("Google One auto-discovery failed: %v", errSetup)
			return
		}
		autoProject := strings.TrimSpace(storage.ProjectID)
		if autoProject == "" {
			log.Error("Google One auto-discovery returned empty project ID")
			return
		}
		log.Infof("Auto-discovered project: %s", autoProject)
		activatedProjects = []string{autoProject}
	} else {
		projects, errProjects := gemini.FetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			log.Errorf("Failed to get project list: %v", errProjects)
			return
		}

		selectedProjectID := promptForProjectSelection(projects, trimmedProjectID, promptFn)
		projectSelections, errSelection := resolveProjectSelections(selectedProjectID, projects)
		if errSelection != nil {
			log.Errorf("Invalid project selection: %v", errSelection)
			return
		}
		if len(projectSelections) == 0 {
			log.Error("No project selected; aborting login.")
			return
		}

		freeUserProjectPrompt := func(requestedProject, returnedProject string) string {
			fmt.Printf("\nGoogle returned a different project ID:\n")
			fmt.Printf("  Requested (frontend): %s\n", requestedProject)
			fmt.Printf("  Returned (backend):   %s\n\n", returnedProject)
			fmt.Printf("  Backend project IDs have access to preview models (gemini-3-*).\n")
			fmt.Printf("  This is normal for free tier users.\n\n")
			fmt.Printf("Which project ID would you like to use?\n")
			fmt.Printf("  [1] Backend (recommended): %s\n", returnedProject)
			fmt.Printf("  [2] Frontend: %s\n\n", requestedProject)
			fmt.Printf("Enter choice [1]: ")

			reader := bufio.NewReader(os.Stdin)
			choice, _ := reader.ReadString('\n')
			choice = strings.TrimSpace(choice)

			if choice == "2" {
				log.Infof("Using frontend project ID: %s", requestedProject)
				fmt.Println(". Warning: Frontend project IDs may not have access to preview models.")
				return requestedProject
			}
			log.Infof("Using backend project ID: %s (recommended)", returnedProject)
			return returnedProject
		}

		seenProjects := make(map[string]bool)
		for _, candidateID := range projectSelections {
			log.Infof("Activating project %s", candidateID)
			if errSetup := gemini.PerformCLISetup(
				ctx, httpClient, storage, candidateID, &gemini.CLISetupOptions{
					OnFreeUserProjectConflict: freeUserProjectPrompt,
				},
			); errSetup != nil {
				if _, ok := errors.AsType[*gemini.ProjectSelectionRequiredError](errSetup); ok {
					log.Error("Failed to start user onboarding: A project ID is required.")
					showProjectSelectionHelp(storage.Email, projects)
					return
				}
				log.Errorf("Failed to complete user setup: %v", errSetup)
				return
			}
			finalID := strings.TrimSpace(storage.ProjectID)
			if finalID == "" {
				finalID = candidateID
			}

			if seenProjects[finalID] {
				log.Infof("Project %s already activated, skipping", finalID)
				continue
			}
			seenProjects[finalID] = true
			activatedProjects = append(activatedProjects, finalID)
		}
	}

	storage.Auto = false
	storage.ProjectID = strings.Join(activatedProjects, ",")

	if !storage.Auto && !storage.Checked {
		for _, pid := range activatedProjects {
			isChecked, errCheck := gemini.CheckCloudAPIIsEnabled(ctx, httpClient, pid)
			if errCheck != nil {
				log.Errorf("Failed to check if Cloud AI API is enabled for %s: %v", pid, errCheck)
				return
			}
			if !isChecked {
				log.Errorf(
					"Failed to check if Cloud AI API is enabled for project %s. If you encounter an error message, please create an issue.",
					pid,
				)
				return
			}
		}
		storage.Checked = true
	}

	updateAuthRecord(record, storage)

	store := sdkAuth.GetTokenStore()
	if setter, okSetter := store.(interface{ SetBaseDir(string) }); okSetter && cfg != nil {
		setter.SetBaseDir(cfg.AuthDir)
	}

	savedPath, errSave := store.Save(ctx, record)
	if errSave != nil {
		log.Errorf("Failed to save token to file: %v", errSave)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}

	fmt.Println("Gemini authentication successful!")
}

// promptForProjectSelection prints available projects and returns the chosen project ID.
func promptForProjectSelection(
	projects []interfaces.GCPProjectProjects,
	presetID string,
	promptFn func(string) (string, error),
) string {
	trimmedPreset := strings.TrimSpace(presetID)
	if len(projects) == 0 {
		if trimmedPreset != "" {
			return trimmedPreset
		}
		fmt.Println("No Google Cloud projects are available for selection.")
		return ""
	}

	fmt.Println("Available Google Cloud projects:")
	defaultIndex := 0
	for idx, project := range projects {
		fmt.Printf("[%d] %s (%s)\n", idx+1, project.ProjectID, project.Name)
		if trimmedPreset != "" && project.ProjectID == trimmedPreset {
			defaultIndex = idx
		}
	}
	fmt.Println("Type 'ALL' to onboard every listed project.")

	defaultID := projects[defaultIndex].ProjectID

	if trimmedPreset != "" {
		if strings.EqualFold(trimmedPreset, "ALL") {
			return "ALL"
		}
		for _, project := range projects {
			if project.ProjectID == trimmedPreset {
				return trimmedPreset
			}
		}
		log.Warnf("Provided project ID %s not found in available projects; please choose from the list.", trimmedPreset)
	}

	for {
		promptMsg := fmt.Sprintf("Enter project ID [%s] or ALL: ", defaultID)
		answer, errPrompt := promptFn(promptMsg)
		if errPrompt != nil {
			log.Errorf("Project selection prompt failed: %v", errPrompt)
			return defaultID
		}
		answer = strings.TrimSpace(answer)
		if strings.EqualFold(answer, "ALL") {
			return "ALL"
		}
		if answer == "" {
			return defaultID
		}

		for _, project := range projects {
			if project.ProjectID == answer {
				return project.ProjectID
			}
		}

		if idx, errAtoi := strconv.Atoi(answer); errAtoi == nil {
			if idx >= 1 && idx <= len(projects) {
				return projects[idx-1].ProjectID
			}
		}

		fmt.Println("Invalid selection, enter a project ID or a number from the list.")
	}
}

func resolveProjectSelections(selection string, projects []interfaces.GCPProjectProjects) ([]string, error) {
	trimmed := strings.TrimSpace(selection)
	if trimmed == "" {
		return nil, nil
	}
	available := make(map[string]struct{}, len(projects))
	ordered := make([]string, 0, len(projects))
	for _, project := range projects {
		id := strings.TrimSpace(project.ProjectID)
		if id == "" {
			continue
		}
		if _, exists := available[id]; exists {
			continue
		}
		available[id] = struct{}{}
		ordered = append(ordered, id)
	}
	if strings.EqualFold(trimmed, "ALL") {
		if len(ordered) == 0 {
			return nil, fmt.Errorf("no projects available for ALL selection")
		}
		return append([]string(nil), ordered...), nil
	}
	parts := strings.Split(trimmed, ",")
	selections := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if len(available) > 0 {
			if _, ok := available[id]; !ok {
				return nil, fmt.Errorf("project %s not found in available projects", id)
			}
		}
		seen[id] = struct{}{}
		selections = append(selections, id)
	}
	return selections, nil
}

func defaultProjectPrompt() func(string) (string, error) {
	reader := bufio.NewReader(os.Stdin)
	return func(prompt string) (string, error) {
		fmt.Print(prompt)
		line, errRead := reader.ReadString('\n')
		if errRead != nil {
			if errors.Is(errRead, io.EOF) {
				return strings.TrimSpace(line), nil
			}
			return "", errRead
		}
		return strings.TrimSpace(line), nil
	}
}

func showProjectSelectionHelp(email string, projects []interfaces.GCPProjectProjects) {
	if email != "" {
		log.Infof("Your account %s needs to specify a project ID.", email)
	} else {
		log.Info("You need to specify a project ID.")
	}

	if len(projects) > 0 {
		fmt.Println("========================================================================")
		for _, p := range projects {
			fmt.Printf("Project ID: %s\n", p.ProjectID)
			fmt.Printf("Project Name: %s\n", p.Name)
			fmt.Println("------------------------------------------------------------------------")
		}
	} else {
		fmt.Println("No active projects were returned for this account.")
	}

	fmt.Printf(
		"Please run this command to login again with a specific project:\n\n%s --login --project_id <project_id>\n",
		os.Args[0],
	)
}

func updateAuthRecord(record *cliproxyauth.Auth, storage *gemini.TokenStorage) {
	if record == nil || storage == nil {
		return
	}

	finalName := gemini.CredentialFileName(storage.Email, storage.ProjectID, true)

	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	record.Metadata["email"] = storage.Email
	record.Metadata["project_id"] = storage.ProjectID
	record.Metadata["auto"] = storage.Auto
	record.Metadata["checked"] = storage.Checked

	record.ID = finalName
	record.FileName = finalName
	record.Storage = storage
}
