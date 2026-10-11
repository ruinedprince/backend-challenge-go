# Roteiro de demonstração

Este roteiro leva menos de cinco minutos e usa somente os serviços do Compose local.

## 1. Subir e validar o ambiente

```powershell
docker compose up -d --build
docker compose --profile tools run --rm migrate -path /migrations -database "postgres://challenge:challenge@postgres:5432/challenge?sslmode=disable" up
Invoke-RestMethod http://localhost:8080/health/ready
```

O último comando deve devolver `status: ready`.

## 2. Obter o token do provedor

```powershell
$token = (Invoke-RestMethod -Method Post `
  -Uri http://localhost:8081/realms/wagering/protocol/openid-connect/token `
  -Authentication Basic -AllowUnencryptedAuthentication -Credential (New-Object pscredential('provider-a',(ConvertTo-SecureString 'provider-a-secret' -AsPlainText -Force))) `
  -Body @{ grant_type = 'client_credentials' }).access_token
$headers = @{ Authorization = "Bearer $token" }
```

O token é emitido pelo Keycloak. A API verifica assinatura, emissor, expiração e o cliente
autorizado antes de aceitar uma operação. O Compose fixa o issuer interno como
`http://keycloak:8080/realms/wagering`, permitindo solicitar o token por `localhost:8081`
e validá-lo pela API dentro da rede Docker.

## 3. Abrir uma carteira e processar uma aposta

```powershell
$wallet = Invoke-RestMethod -Method Post -Uri http://localhost:8080/wallets `
  -Headers $headers -ContentType 'application/json' -Body @'
{"playerId":"11111111-1111-4111-8111-111111111111","initialBalance":{"amount":"100.00","currency":"BRL"}}
'@

$bet = @{
  providerId = 'provider-a'; externalTransactionId = 'demo-bet-1'
  playerId = '11111111-1111-4111-8111-111111111111'; walletId = $wallet.id
  roundId = 'demo-round'; gameId = 'demo-game'; kind = 'BET'
  money = @{ amount = '25.00'; currency = 'BRL' }
} | ConvertTo-Json -Depth 3

Invoke-RestMethod -Method Post -Uri http://localhost:8080/wagering/transactions `
  -Headers (@{ Authorization = "Bearer $token"; 'Idempotency-Key' = 'provider-a:demo-bet-1' }) `
  -ContentType 'application/json' -Body $bet
```

O resultado deve ficar em `PROCESSED` com saldo `75.00`. O mesmo request, com a mesma chave,
é um replay idempotente: retorna o resultado já persistido e não cria outro lançamento.

```powershell
Invoke-RestMethod -Method Post -Uri http://localhost:8080/wagering/transactions `
  -Headers (@{ Authorization = "Bearer $token"; 'Idempotency-Key' = 'provider-a:demo-bet-1' }) `
  -ContentType 'application/json' -Body $bet

Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wallets/$($wallet.id)/reconciliation" -Headers $headers
```

## 4. Demonstração de referência pendente

Envie um `REFUND` com `referenceExternalTransactionId` apontando para uma aposta que ainda não
chegou. A resposta fica em `PENDING_REFERENCE`. Quando a aposta referenciada é recebida, o
resolvedor durável a conclui; se ela nunca chegar, após cinco tentativas a operação vira
`REJECTED` com `REFERENCE_NOT_FOUND`.

## 5. Evidências automatizadas

```powershell
make verify
```

Esse comando roda testes unitários, detector de corrida, análise estática e integração com
PostgreSQL/LocalStack reais. A integração cobre disputa de saldo, 50 replays paralelos,
redelivery da inbox, referências pendentes/expiradas e três publishers concorrentes da outbox.

## Limitações conhecidas

- A listagem do ledger é limitada aos 50 registros mais recentes; paginação opaca não foi incluída.
- Métricas e tracing distribuído não foram implementados; há logs JSON estruturados.
- A entrega de eventos é at-least-once. Consumidores de `wager-events.fifo` devem deduplicar por
  `eventId`, que permanece estável em republicações.
