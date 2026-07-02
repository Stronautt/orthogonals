# orthogonals guest provisioning - runs from the ORTHOGONALS ISO at first
# logon and re-runs via a scheduled task until every stage is done. Each
# stage is idempotent: a done-check skips it once it holds, so reboots and
# re-entry mid-way are safe. Progress lands in provision-status.json, which
# the host polls through the qemu guest agent.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$workDir = 'C:\orthogonals'
$vddDir = 'C:\VirtualDisplayDriver'   # fixed path: VDD reads its settings here
$statusPath = Join-Path $workDir 'provision-status.json'
$debloatStamp = Join-Path $workDir 'debloat.done'
$lgService = '{{.LGService}}'
$taskName = 'orthogonals-provision'
$lgRestartTask = 'orthogonals-lg-restart'

New-Item -ItemType Directory -Force -Path $workDir | Out-Null
Start-Transcript -Path (Join-Path $workDir 'provision.log') -Append | Out-Null

# the host-side poller's contract: {stage, ok, error}, ASCII, one line
function Write-Status([string]$Stage, [bool]$Ok, [string]$Err = '') {
    $json = [ordered]@{ stage = $Stage; ok = $Ok; error = $Err } | ConvertTo-Json -Compress
    Set-Content -Path $statusPath -Value $json -Encoding ascii
}

# drive letters vary per boot, so files are located by searching every CD
# (the ORTHOGONALS provision ISO and the virtio-win ISO both ride as CDs)
function Find-CDFile([string]$Name) {
    foreach ($v in Get-Volume | Where-Object DriveType -eq 'CD-ROM') {
        if (-not $v.DriveLetter) { continue }
        $p = "$($v.DriveLetter):\$Name"
        if (Test-Path $p) { return $p }
    }
    throw "no attached CD carries $Name"
}

function Get-NvidiaGpu {
    Get-CimInstance Win32_VideoController |
        Where-Object { $_.Name -like '*NVIDIA*' } | Select-Object -First 1
}

function Invoke-Installer([string]$Path, [string]$Arguments) {
    (Start-Process -FilePath $Path -ArgumentList $Arguments -Wait -PassThru).ExitCode
}

# A driver whose publisher Windows cannot verify raises a modal Windows
# Security prompt that no silent installer can answer - unattended
# provisioning hangs on it forever. Trusting the signer up front is what
# keeps the install headless.
function Add-TrustedPublisher([string]$Path) {
    $signer = (Get-AuthenticodeSignature $Path).SignerCertificate
    if (-not $signer) { throw "$Path is not Authenticode-signed" }
    foreach ($storeName in 'TrustedPublisher', 'Root') {
        $store = New-Object System.Security.Cryptography.X509Certificates.X509Store($storeName, 'LocalMachine')
        $store.Open('ReadWrite'); $store.Add($signer); $store.Close()
    }
}

Write-Status -Stage 'start' -Ok $true

# Windows Update ships its own NVIDIA driver and races the pinned one: the
# guest lands on an unpinned version, the concurrent device install throws a
# modal trust prompt no silent installer can answer, and the box reboots
# mid-stage. Provisioning owns the drivers; the driver-search policy stays off
# afterwards (a WU driver swap would break the VDD/Looking Glass pairing) while
# the cleanup stage hands the update service back for security updates.
function Set-RegValue([string]$Path, [string]$Name, [int]$Value) {
    if (-not (Test-Path $Path)) { New-Item -Path $Path -Force | Out-Null }
    Set-ItemProperty -Path $Path -Name $Name -Value $Value -Type DWord
}
Set-RegValue 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\DriverSearching' 'SearchOrderConfig' 0
Set-RegValue 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\DriverSearching' 'DontSearchWindowsUpdate' 1
Set-RegValue 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate' 'ExcludeWUDriversInQualityUpdate' 1
Stop-Service wuauserv -Force -ErrorAction SilentlyContinue
Set-Service wuauserv -StartupType Disabled -ErrorAction SilentlyContinue

# re-entry: installs can reboot the guest, so a logon task re-runs this
# script (autounattend.xml's AutoLogon keeps those logons unattended) until
# the cleanup stage removes it
if (-not (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue)) {
    $runner = Join-Path $workDir 'provision-run.ps1'
    Set-Content -Path $runner -Encoding ascii -Value @'
$d = (Get-Volume -FileSystemLabel 'ORTHOGONALS').DriveLetter
& ($d + ':\provision.ps1')
'@
    $action = New-ScheduledTaskAction -Execute 'powershell.exe' `
        -Argument ('-NoProfile -ExecutionPolicy Bypass -File "' + $runner + '"')
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User '{{ps .GuestUser}}'
    $principal = New-ScheduledTaskPrincipal -UserId '{{ps .GuestUser}}' -RunLevel Highest
    Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal | Out-Null
}

$script:needReboot = $false

$stages = @(
    @{
        # the guest-tools bundle chains the driver MSI, QEMU-GA and the SPICE
        # vdagent; the bare virtio-win-gt MSI is drivers only — no vdagent, no
        # clipboard. QEMU-GA is the only channel the host polls provisioning
        # through; spice-agent is what moves the clipboard over SPICE.
        Name = 'virtio-guest-tools'
        Done = { (Get-Service -Name 'QEMU-GA' -ErrorAction SilentlyContinue) -and
                 (Get-Service -Name 'spice-agent' -ErrorAction SilentlyContinue) }
        Run  = {
            $exe = Find-CDFile 'virtio-win-guest-tools.exe'
            $code = Invoke-Installer $exe '/install /quiet /norestart'
            if ($code -eq 3010) { $script:needReboot = $true }   # ERROR_SUCCESS_REBOOT_REQUIRED
            elseif ($code -ne 0) { throw "virtio guest tools exit code $code" }
        }
    },
    @{
        # before the driver lands the GPU shows as Microsoft Basic Display
        # Adapter, never as NVIDIA - that is the done-check
        Name = 'nvidia-driver'
        Done = { Get-NvidiaGpu }
        Run  = {
            # no publisher trust needed: every catalog in the package is
            # WHQL-signed, unlike the VDD's
            $exe = Find-CDFile 'nvidia-driver.exe'
            $code = Invoke-Installer $exe '-s -noreboot'
            if ($code -eq 1) { $script:needReboot = $true }   # NVIDIA setup: 1 = reboot required
            # Any other nonzero code is not trustworthy: the installer restarts
            # the guest mid-install on its own, and the re-run that lands on the
            # half-finished install reports failure while completing the job.
            # The device is the verdict, not the code.
            elseif ($code -ne 0 -and -not (Get-NvidiaGpu)) { throw "NVIDIA installer exit code $code" }
        }
    },
    @{
        Name = 'looking-glass-host'   # /S bundles the IVSHMEM driver since B6
        Done = { Test-Path "$env:ProgramFiles\Looking Glass (host)\looking-glass-host.exe" }
        Run  = {
            $dir = Join-Path $workDir 'looking-glass'
            Expand-Archive -Path (Find-CDFile 'looking-glass-host.zip') -DestinationPath $dir -Force
            $setup = Get-ChildItem -Path $dir -Recurse -Filter 'looking-glass-host-setup.exe' | Select-Object -First 1
            if (-not $setup) { throw 'looking-glass-host.zip does not contain looking-glass-host-setup.exe' }
            $code = Invoke-Installer $setup.FullName '/S'
            if ($code -ne 0) { throw "Looking Glass host setup exit code $code" }
        }
    },
    @{
        Name = 'vdd'   # the virtual monitor the passed-through GPU renders to
        Done = {
            Get-PnpDevice -ErrorAction SilentlyContinue |
                Where-Object { $_.HardwareID -contains '{{.VDDHardwareID}}' -and $_.Status -eq 'OK' }
        }
        Run  = {
            # pin the settings to the passed-through GPU: host-side detect only
            # knows PCI IDs, the friendly name exists only in-guest
            $gpu = Get-NvidiaGpu
            if (-not $gpu) { throw 'no NVIDIA adapter to pin the virtual display to' }
            Expand-Archive -Path (Find-CDFile 'vdd-driver.zip') -DestinationPath $vddDir -Force
            (Get-Content (Find-CDFile 'vdd_settings.xml')) -replace 'GPU_FRIENDLY_NAME', $gpu.Name |
                Set-Content -Path (Join-Path $vddDir 'vdd_settings.xml') -Encoding ascii

            # signed third-party driver (not WHQL): trust its signer so the
            # driver install below runs without a prompt
            $cat = Get-ChildItem -Path $vddDir -Recurse -Filter '*.cat' | Select-Object -First 1
            if (-not $cat) { throw 'vdd-driver.zip has no signed catalog (*.cat)' }
            Add-TrustedPublisher $cat.FullName

            # nefcon installs the inf and creates the device node reboot-free
            $nefconDir = Join-Path $workDir 'nefcon'
            Expand-Archive -Path (Find-CDFile 'nefcon.zip') -DestinationPath $nefconDir -Force
            $nefcon = Get-ChildItem -Path $nefconDir -Recurse -Filter 'nefconc.exe' |
                Where-Object { $_.FullName -match 'x64' } | Select-Object -First 1
            if (-not $nefcon) { $nefcon = Get-ChildItem -Path $nefconDir -Recurse -Filter 'nefconc.exe' | Select-Object -First 1 }
            if (-not $nefcon) { throw 'nefcon.zip does not contain nefconc.exe' }
            $inf = Get-ChildItem -Path $vddDir -Recurse -Filter '*.inf' | Select-Object -First 1
            if (-not $inf) { throw 'vdd-driver.zip does not contain an inf' }

            $node = Get-PnpDevice -ErrorAction SilentlyContinue |
                Where-Object { $_.HardwareID -contains '{{.VDDHardwareID}}' }
            if (-not $node) {
                & $nefcon.FullName --create-device-node --hardware-id '{{.VDDHardwareID}}' `
                    --class-name Display --class-guid '4d36e968-e325-11ce-bfc1-08002be10318'
                if ($LASTEXITCODE -ne 0) { throw "nefcon create-device-node exit code $LASTEXITCODE" }
            }
            & $nefcon.FullName --install-driver --inf-path $inf.FullName
            if ($LASTEXITCODE -ne 0) { throw "nefcon install-driver exit code $LASTEXITCODE" }
        }
    },
    @{
        Name = 'lg-service'
        Done = {
            $svc = Get-Service -Name $lgService -ErrorAction SilentlyContinue
            $svc -and $svc.StartType -eq 'Automatic' -and $svc.Status -eq 'Running' -and
                (Get-ScheduledTask -TaskName $lgRestartTask -ErrorAction SilentlyContinue)
        }
        Run  = {
            Set-Service -Name $lgService -StartupType Automatic
            Start-Service -Name $lgService
            # the service starts early in boot and can latch its capture onto
            # a display topology Windows is still settling — the client then
            # connects but never receives a frame. Whether frames flow is only
            # observable host-side, so no in-guest check can gate this: one
            # unconditional restart after boot re-inits capture against the
            # settled topology (a viewer connected within the delay sees a
            # sub-second reconnect blip).
            $action = New-ScheduledTaskAction -Execute 'powershell.exe' `
                -Argument ('-NoProfile -Command "Restart-Service ''' + $lgService + '''"')
            $trigger = New-ScheduledTaskTrigger -AtStartup
            $trigger.Delay = 'PT30S'
            $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
            Register-ScheduledTask -TaskName $lgRestartTask -Action $action -Trigger $trigger -Principal $principal -Force | Out-Null
        }
    },
    @{
        # Win11Debloat's defaults: strip the preinstalled consumer apps,
        # telemetry, ads and suggestions. They leave the gaming apps alone
        # (-RemoveGamingApps is a separate opt-in), which is what this guest is
        # for. Runs last, so a debloat regression can never disturb the drivers.
        Name = 'debloat'
        # the script leaves no single state of its own to probe, so the stage
        # records its own completion
        Done = { Test-Path $debloatStamp }
        Run  = {
            $dir = Join-Path $workDir 'win11debloat'
            Expand-Archive -Path (Find-CDFile 'win11debloat.zip') -DestinationPath $dir -Force
            $script = Get-ChildItem -Path $dir -Recurse -Filter 'Win11Debloat.ps1' | Select-Object -First 1
            if (-not $script) { throw 'win11debloat.zip does not contain Win11Debloat.ps1' }
            # -NoRestartExplorer: the settings are registry-deep and the guest
            # reboots at the end of verify anyway
            & $script.FullName -Silent -RunDefaults -NoRestartExplorer
            # stamp regardless of the exit code (fail-open: a benign nonzero
            # exit from upstream must not wedge provisioning in a retry loop)
            # but record it, so a failed debloat is observable post-hoc
            Set-Content -Path $debloatStamp -Value "exit=$LASTEXITCODE"
        }
    },
    @{
        Name = 'cleanup'   # every action below is a no-op when re-run
        Done = { $false }
        SkipVerify = $true   # Done is false by design, so it always re-runs
        Run  = {
            # Windows Setup caches the answer file WITH the admin password and
            # it persists after install (research §B2, known 24H2 issue)
            Remove-Item -Force -ErrorAction SilentlyContinue `
                'C:\Windows\Panther\unattend.xml', 'C:\Windows\System32\Sysprep\unattend.xml'
            # autologon has served its purpose - it too stores the password
            $winlogon = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
            Set-ItemProperty -Path $winlogon -Name AutoAdminLogon -Value '0'
            Remove-ItemProperty -Path $winlogon -Name DefaultPassword -ErrorAction SilentlyContinue
            Remove-ItemProperty -Path $winlogon -Name AutoLogonCount -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
            # security updates resume; only the driver-search policy stays off
            Set-Service wuauserv -StartupType Manual -ErrorAction SilentlyContinue
            Start-Service wuauserv -ErrorAction SilentlyContinue
        }
    }
)

foreach ($s in $stages) {
    if (& $s.Done) {
        Write-Output "stage $($s.Name): already done"
        continue
    }
    Write-Output "stage $($s.Name): running"
    try {
        & $s.Run
    } catch {
        Write-Status -Stage $s.Name -Ok $false -Err $_.Exception.Message
        Stop-Transcript | Out-Null
        exit 1
    }
    # an installer that exits 0 without reaching the stage's done state would
    # otherwise mark the stage green and leave the host polling a guest that
    # never finishes - fail loudly instead. Skipped when a reboot is pending:
    # those stages only reach their done state after it.
    if (-not $s.SkipVerify -and -not $script:needReboot -and -not (& $s.Done)) {
        $err = 'installer reported success but the stage did not take effect'
        Write-Output "stage $($s.Name): $err"
        Write-Status -Stage $s.Name -Ok $false -Err $err
        Stop-Transcript | Out-Null
        exit 1
    }
    Write-Status -Stage $s.Name -Ok $true
    if ($script:needReboot) {
        Write-Output "stage $($s.Name): reboot required - restarting, provisioning resumes at logon"
        Stop-Transcript | Out-Null
        Restart-Computer -Force
        exit 0
    }
}

Write-Status -Stage 'done' -Ok $true
Write-Output 'provisioning complete'
Stop-Transcript | Out-Null
