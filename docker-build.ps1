# build.ps1 - Windows PowerShell Build Script
#
# This script automates the process of building and running the Docker container
# with version information dynamically injected at build time.

# Stop script execution on any error
$ErrorActionPreference = "Stop"

$configDir = $env:CLI_PROXY_CONFIG_DIR
if ([string]::IsNullOrWhiteSpace($configDir)) {
    if (Test-Path -LiteralPath "./config/config.yaml" -PathType Leaf) {
        $configDir = "./config"
    }
    elseif (Test-Path -LiteralPath "./config.yaml" -PathType Leaf) {
        $configDir = "."
        $env:CLI_PROXY_CONFIG_DIR = "."
        Write-Warning "Using legacy ./config.yaml. Move it to ./config/config.yaml for the new default layout."
    }
    else {
        throw "Config file not found. Run: New-Item -ItemType Directory -Force config; Copy-Item config.example.yaml config/config.yaml"
    }
}

$configFile = Join-Path $configDir "config.yaml"
if (-not (Test-Path -LiteralPath $configFile -PathType Leaf)) {
    throw "Config file not found: $configFile"
}

# --- Step 1: Choose Environment ---
Write-Host "Please select an option:"
Write-Host "1) Run using Pre-built Image (Recommended)"
Write-Host "2) Build from Source and Run (For Developers)"
$choice = Read-Host -Prompt "Enter choice [1-2]"

# --- Step 2: Execute based on choice ---
switch ($choice) {
    "1" {
        Write-Host "--- Running with Pre-built Image ---"
        docker compose up -d --remove-orphans --no-build
        Write-Host "Services are starting from remote image."
        Write-Host "Run 'docker compose logs -f' to see the logs."
    }
    "2" {
        Write-Host "--- Building from Source and Running ---"

        # Get Version Information
        $VERSION = (git describe --tags --always --dirty)
        $COMMIT  = (git rev-parse --short HEAD)
        $BUILD_DATE = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

        Write-Host "Building with the following info:"
        Write-Host "  Version: $VERSION"
        Write-Host "  Commit: $COMMIT"
        Write-Host "  Build Date: $BUILD_DATE"
        Write-Host "----------------------------------------"

        # Build and start the services with a local-only image tag
        $env:CLI_PROXY_IMAGE = "cli-proxy-api:local"
        
        Write-Host "Preparing management panel asset..."
        & "$PSScriptRoot/scripts/prepare-management-panel.ps1"

        Write-Host "Building the Docker image..."
        docker compose build --build-arg VERSION=$VERSION --build-arg COMMIT=$COMMIT --build-arg BUILD_DATE=$BUILD_DATE

        Write-Host "Starting the services..."
        docker compose up -d --remove-orphans --pull never

        Write-Host "Build complete. Services are starting."
        Write-Host "Run 'docker compose logs -f' to see the logs."
    }
    default {
        Write-Host "Invalid choice. Please enter 1 or 2."
        exit 1
    }
}