$ErrorActionPreference = 'Stop'

Set-Location (Join-Path $PSScriptRoot '..')

function Assert-That {
    param(
        [bool]$Condition,
        [string]$Message
    )

    if (-not $Condition) {
        throw $Message
    }
}

function Wait-ForReady {
    param([string]$BaseUrl)

    for ($attempt = 1; $attempt -le 60; $attempt++) {
        try {
            $health = Invoke-RestMethod "$BaseUrl/health/ready"
            if ($health.status -eq 'ready') {
                return
            }
        } catch {
            # The container may still be starting or waiting for Keycloak.
        }
        Start-Sleep -Seconds 1
    }

    throw "API at $BaseUrl did not become ready within 60 seconds."
}

function New-Wallet {
    param(
        [string]$BaseUrl,
        [hashtable]$Headers
    )

    $body = @{
        playerId       = [guid]::NewGuid().ToString()
        initialBalance = @{ amount = '100.00'; currency = 'BRL' }
    } | ConvertTo-Json -Depth 3

    Invoke-RestMethod -Method Post -Uri "$BaseUrl/wallets" -Headers $Headers -ContentType 'application/json' -Body $body
}

function New-BetBody {
    param(
        [string]$WalletId,
        [string]$PlayerId,
        [string]$ExternalTransactionId,
        [string]$Amount
    )

    @{
        providerId            = 'provider-a'
        externalTransactionId = $ExternalTransactionId
        playerId              = $PlayerId
        walletId              = $WalletId
        roundId               = 'multi-instance-round'
        gameId                = 'multi-instance-game'
        kind                  = 'BET'
        money                 = @{ amount = $Amount; currency = 'BRL' }
    } | ConvertTo-Json -Depth 3 -Compress
}

Write-Host 'Starting the three API instances and their shared dependencies...'
docker compose up -d --build
if ($LASTEXITCODE -ne 0) { throw 'docker compose up failed.' }

docker compose --profile tools run --rm migrate -path /migrations -database 'postgres://challenge:challenge@postgres:5432/challenge?sslmode=disable' up
if ($LASTEXITCODE -ne 0) { throw 'database migrations failed.' }

$apiUrls = @('http://localhost:8080', 'http://localhost:8082', 'http://localhost:8083')
foreach ($apiUrl in $apiUrls) {
    Wait-ForReady $apiUrl
}
Write-Host 'API A, API B and API C are ready.'

$token = (Invoke-RestMethod -Method Post `
    -Uri 'http://localhost:8081/realms/wagering/protocol/openid-connect/token' `
    -Authentication Basic -AllowUnencryptedAuthentication `
    -Credential (New-Object pscredential('provider-a', (ConvertTo-SecureString 'provider-a-secret' -AsPlainText -Force))) `
    -Body @{ grant_type = 'client_credentials' }).access_token

$headers = @{ Authorization = "Bearer $token" }
$runId = [guid]::NewGuid().ToString()

# Scenario 1: two different bets for the same R$100 wallet reach API A and API B together.
$wallet = New-Wallet -BaseUrl $apiUrls[0] -Headers $headers
$firstBet = New-BetBody -WalletId $wallet.id -PlayerId $wallet.playerId -ExternalTransactionId "multi-a-$([guid]::NewGuid())" -Amount '80.00'
$secondBet = New-BetBody -WalletId $wallet.id -PlayerId $wallet.playerId -ExternalTransactionId "multi-b-$([guid]::NewGuid())" -Amount '80.00'
$firstKey = "multi-instance-a-$runId"
$secondKey = "multi-instance-b-$runId"

$contentType = 'application/json'
$firstJob = Start-Job -ScriptBlock {
    param($Token, $Body, $ContentType, $Key)
    Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/wagering/transactions' -Headers @{ Authorization = "Bearer $Token"; 'Idempotency-Key' = $Key } -ContentType $ContentType -Body $Body
} -ArgumentList $token, $firstBet, $contentType, $firstKey
$secondJob = Start-Job -ScriptBlock {
    param($Token, $Body, $ContentType, $Key)
    Invoke-RestMethod -Method Post -Uri 'http://localhost:8082/wagering/transactions' -Headers @{ Authorization = "Bearer $Token"; 'Idempotency-Key' = $Key } -ContentType $ContentType -Body $Body
} -ArgumentList $token, $secondBet, $contentType, $secondKey

Wait-Job $firstJob, $secondJob | Out-Null
$contestedResults = @($firstJob, $secondJob | ForEach-Object { Receive-Job $_ -ErrorAction Stop })
Remove-Job $firstJob, $secondJob

$processed = @($contestedResults | Where-Object status -eq 'PROCESSED').Count
$rejected = @($contestedResults | Where-Object status -eq 'REJECTED').Count
$contestedWallet = Invoke-RestMethod -Uri "$($apiUrls[2])/wallets/$($wallet.id)" -Headers $headers
$contestedLedger = Invoke-RestMethod -Uri "$($apiUrls[2])/wallets/$($wallet.id)/ledger" -Headers $headers
$contestedDebits = @($contestedLedger.entries | Where-Object Direction -eq 'DEBIT').Count

Assert-That ($processed -eq 1 -and $rejected -eq 1) 'Expected one processed and one rejected R$80 bet.'
Assert-That ($contestedWallet.balance.amount -eq '20.00') 'Expected contested wallet balance to be R$20.00.'
Assert-That ($contestedDebits -eq 1) 'Expected exactly one debit in the contested wallet ledger.'
Write-Host 'PASS: API A and API B contested one wallet; API C confirmed R$20.00 and one debit.'

# Scenario 2: independent wallets continue in parallel across all three APIs.
$parallelWallets = @(
    (New-Wallet -BaseUrl $apiUrls[0] -Headers $headers)
    (New-Wallet -BaseUrl $apiUrls[1] -Headers $headers)
    (New-Wallet -BaseUrl $apiUrls[2] -Headers $headers)
)
$parallelJobs = foreach ($number in 0..2) {
    $parallelWallet = $parallelWallets[$number]
    $parallelBody = New-BetBody -WalletId $parallelWallet.id -PlayerId $parallelWallet.playerId -ExternalTransactionId "parallel-$number-$([guid]::NewGuid())" -Amount '25.00'
    $url = $apiUrls[$number]
    Start-Job -ScriptBlock {
        param($Url, $Token, $Body, $ContentType, $Key)
        Invoke-RestMethod -Method Post -Uri "$Url/wagering/transactions" -Headers @{ Authorization = "Bearer $Token"; 'Idempotency-Key' = $Key } -ContentType $ContentType -Body $Body
    } -ArgumentList $url, $token, $parallelBody, $contentType, "parallel-$runId-$number"
}

Wait-Job $parallelJobs | Out-Null
$parallelResults = @($parallelJobs | ForEach-Object { Receive-Job $_ -ErrorAction Stop })
Remove-Job $parallelJobs
$parallelBalances = @($parallelWallets | ForEach-Object {
    (Invoke-RestMethod -Uri "$($apiUrls[2])/wallets/$($_.id)" -Headers $headers).balance.amount
})

Assert-That (@($parallelResults | Where-Object status -eq 'PROCESSED').Count -eq 3) 'Expected three independent-wallet bets to be processed.'
Assert-That (@($parallelBalances | Where-Object { $_ -ne '75.00' }).Count -eq 0) 'Expected every independent wallet balance to be R$75.00.'
Write-Host 'PASS: API A, API B and API C processed different wallets in parallel.'

# Scenario 3: fifty deliveries of the same bet are spread across all three APIs.
$replayWallet = New-Wallet -BaseUrl $apiUrls[0] -Headers $headers
$replayExternalId = "multi-replay-$([guid]::NewGuid())"
$replayBody = New-BetBody -WalletId $replayWallet.id -PlayerId $replayWallet.playerId -ExternalTransactionId $replayExternalId -Amount '25.00'
$replayKey = "multi-instance-replay-$runId"
$replayJobs = foreach ($number in 0..49) {
    $url = $apiUrls[$number % $apiUrls.Count]
    Start-Job -ScriptBlock {
        param($Url, $Token, $Body, $ContentType, $Key)
        Invoke-RestMethod -Method Post -Uri "$Url/wagering/transactions" -Headers @{ Authorization = "Bearer $Token"; 'Idempotency-Key' = $Key } -ContentType $ContentType -Body $Body
    } -ArgumentList $url, $token, $replayBody, $contentType, $replayKey
}

Wait-Job $replayJobs | Out-Null
$replayResults = @($replayJobs | ForEach-Object { Receive-Job $_ -ErrorAction Stop })
Remove-Job $replayJobs

$replayWalletState = Invoke-RestMethod -Uri "$($apiUrls[2])/wallets/$($replayWallet.id)" -Headers $headers
$replayLedger = Invoke-RestMethod -Uri "$($apiUrls[2])/wallets/$($replayWallet.id)/ledger" -Headers $headers
$replayDebits = @($replayLedger.entries | Where-Object Direction -eq 'DEBIT').Count
$firstDeliveries = @($replayResults | Where-Object { -not $_.idempotentReplay }).Count
$repeatedDeliveries = @($replayResults | Where-Object idempotentReplay).Count

Assert-That (@($replayResults | Where-Object status -eq 'PROCESSED').Count -eq 50) 'Expected every replay request to return PROCESSED.'
Assert-That ($firstDeliveries -eq 1 -and $repeatedDeliveries -eq 49) 'Expected one first delivery and forty-nine idempotent replays.'
Assert-That ($replayWalletState.balance.amount -eq '75.00') 'Expected replay wallet balance to be R$75.00.'
Assert-That ($replayDebits -eq 1) 'Expected exactly one debit in the replay wallet ledger.'
Write-Host 'PASS: 50 copies through API A, B and C produced one R$25.00 debit and 49 idempotent replays.'
Write-Host 'Multi-instance demonstration completed successfully.'
