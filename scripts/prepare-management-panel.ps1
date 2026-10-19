param(
    [string]$PanelRepo,
    [string]$PanelTag,
    [string]$OutputPath,
    [string]$SourcePath
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($PanelRepo)) {
    $PanelRepo = if ([string]::IsNullOrWhiteSpace($env:PANEL_REPO)) {
        "Pyrokine/Cli-Proxy-API-Management-Center"
    }
    else {
        $env:PANEL_REPO
    }
}
if ([string]::IsNullOrWhiteSpace($PanelTag)) {
    $PanelTag = if ([string]::IsNullOrWhiteSpace($env:PANEL_TAG)) {
        "v1.26.0-aug.1"
    }
    else {
        $env:PANEL_TAG
    }
}
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = if ([string]::IsNullOrWhiteSpace($env:PANEL_OUTPUT)) {
        "static/management.html"
    }
    else {
        $env:PANEL_OUTPUT
    }
}

if ([string]::IsNullOrWhiteSpace($SourcePath) -and -not [string]::IsNullOrWhiteSpace($env:PANEL_SOURCE)) {
    $SourcePath = $env:PANEL_SOURCE
}

$outputDir = Split-Path -Parent $OutputPath
if ([string]::IsNullOrWhiteSpace($outputDir)) {
    $outputDir = "."
}
New-Item -ItemType Directory -Force -Path $outputDir | Out-Null
$tempAsset = Join-Path $outputDir ".management.$([guid]::NewGuid().ToString('N'))"

try {
    if (-not [string]::IsNullOrWhiteSpace($SourcePath)) {
        if (-not (Test-Path -LiteralPath $SourcePath -PathType Leaf)) {
            throw "Management panel source not found: $SourcePath"
        }
        Copy-Item -LiteralPath $SourcePath -Destination $tempAsset -Force
        $sourceDescription = $SourcePath
    }
    else {
        $headers = @{
            Accept = "application/vnd.github+json"
            "User-Agent" = "cli-proxy-api-docker-build"
        }
        if (-not [string]::IsNullOrWhiteSpace($env:GH_TOKEN)) {
            $headers.Authorization = "Bearer $($env:GH_TOKEN)"
        }

        $releaseUrl = "https://api.github.com/repos/$PanelRepo/releases/tags/$PanelTag"
        $release = Invoke-RestMethod -Method Get -Uri $releaseUrl -Headers $headers
        if ($release.tag_name -ne $PanelTag) {
            throw "Management panel tag mismatch: expected $PanelTag, got '$($release.tag_name)'"
        }

        $asset = $release.assets | Where-Object {
            $_.name -eq "management.html" -and -not [string]::IsNullOrWhiteSpace($_.browser_download_url)
        } | Select-Object -First 1
        if ($null -eq $asset) {
            throw "management.html release asset not found for $PanelTag"
        }
        Invoke-WebRequest -Uri $asset.browser_download_url -Headers $headers -OutFile $tempAsset
        $sourceDescription = "$PanelRepo@$PanelTag"
    }

    if ((Get-Item -LiteralPath $tempAsset).Length -eq 0) {
        throw "Downloaded management.html is empty"
    }
    if (Select-String -LiteralPath $tempAsset -SimpleMatch -Quiet "Management panel asset is not included in this Docker image") {
        throw "Downloaded management.html is the fallback page"
    }
    Move-Item -LiteralPath $tempAsset -Destination $OutputPath -Force
}
finally {
    if (Test-Path -LiteralPath $tempAsset) {
        Remove-Item -LiteralPath $tempAsset -Force
    }
}

Write-Host "Prepared $OutputPath from $sourceDescription"
