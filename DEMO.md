# Roteiro de demonstração

Este roteiro leva menos de cinco minutos e usa somente os serviços do Compose local.

## 1. Subir e validar o ambiente

```powershell
docker compose up -d --build
docker compose --profile tools run --rm migrate -path /migrations -database "postgres://challenge:challenge@postgres:5432/challenge?sslmode=disable" up
Invoke-RestMethod http://localhost:8080/health/ready
Invoke-RestMethod http://localhost:8082/health/ready
Invoke-RestMethod http://localhost:8083/health/ready
```

Os três comandos devem devolver `status: ready`. O Compose inicia três cópias idênticas da API:

| Serviço | Porta local | Papel na demonstração |
| --- | --- | --- |
| `api-a` | `8080` | Primeira instância |
| `api-b` | `8082` | Segunda instância |
| `api-c` | `8083` | Terceira instância |

Elas têm memória e conexões próprias, mas usam o mesmo PostgreSQL, Keycloak e LocalStack. As
portas distintas existem apenas no computador local para o roteiro enviar uma requisição para
uma instância escolhida. Não representam três APIs diferentes.

## 2. Obter o token do provedor

```powershell
$providerToken = (Invoke-RestMethod -Method Post `
  -Uri http://localhost:8081/realms/wagering/protocol/openid-connect/token `
  -Authentication Basic -AllowUnencryptedAuthentication -Credential (New-Object pscredential('provider-a',(ConvertTo-SecureString 'provider-a-secret' -AsPlainText -Force))) `
  -Body @{ grant_type = 'client_credentials' }).access_token
$providerHeaders = @{ Authorization = "Bearer $providerToken" }

$internalToken = (Invoke-RestMethod -Method Post `
  -Uri http://localhost:8081/realms/wagering/protocol/openid-connect/token `
  -Authentication Basic -AllowUnencryptedAuthentication -Credential (New-Object pscredential('wallet-service',(ConvertTo-SecureString 'wallet-service-secret' -AsPlainText -Force))) `
  -Body @{ grant_type = 'client_credentials' }).access_token
$internalHeaders = @{ Authorization = "Bearer $internalToken" }
```

O token é emitido pelo Keycloak. `provider-a` só registra e consulta suas próprias transações;
`wallet-service` é a identidade interna que abre carteiras, consulta ledger e reconcilia saldo.
A API verifica assinatura, emissor, expiração e o cliente autorizado antes de aceitar uma
operação. O Compose fixa o issuer interno como
`http://keycloak:8080/realms/wagering`, permitindo solicitar o token por `localhost:8081`
e validá-lo pela API dentro da rede Docker.

## 3. Abrir uma carteira e processar uma aposta

```powershell
$wallet = Invoke-RestMethod -Method Post -Uri http://localhost:8080/wallets `
  -Headers $internalHeaders -ContentType 'application/json' -Body @'
{"providerId":"provider-a","playerId":"11111111-1111-4111-8111-111111111111","initialBalance":{"amount":"100.00","currency":"BRL"}}
'@

$bet = @{
  providerId = 'provider-a'; externalTransactionId = 'demo-bet-1'
  playerId = '11111111-1111-4111-8111-111111111111'; walletId = $wallet.id
  roundId = 'demo-round'; gameId = 'demo-game'; kind = 'BET'
  money = @{ amount = '25.00'; currency = 'BRL' }
} | ConvertTo-Json -Depth 3

Invoke-RestMethod -Method Post -Uri http://localhost:8080/wagering/transactions `
  -Headers (@{ Authorization = "Bearer $providerToken"; 'Idempotency-Key' = 'provider-a:demo-bet-1' }) `
  -ContentType 'application/json' -Body $bet
```

O resultado deve ficar em `PROCESSED` com saldo `75.00`. O mesmo request, com a mesma chave,
é um replay idempotente: retorna o resultado já persistido e não cria outro lançamento.

```powershell
Invoke-RestMethod -Method Post -Uri http://localhost:8080/wagering/transactions `
  -Headers (@{ Authorization = "Bearer $providerToken"; 'Idempotency-Key' = 'provider-a:demo-bet-1' }) `
  -ContentType 'application/json' -Body $bet

Invoke-RestMethod -Method Post -Uri "http://localhost:8080/wallets/$($wallet.id)/reconciliation" -Headers $internalHeaders
```

## 4. Demonstração de referência pendente

Envie um `REFUND` com `referenceExternalTransactionId` apontando para uma aposta que ainda não
chegou. A resposta fica em `PENDING_REFERENCE`. Quando a aposta referenciada é recebida, o
resolvedor durável a conclui; se ela nunca chegar, após cinco tentativas a operação vira
`REJECTED` com `REFERENCE_NOT_FOUND`.

## 5. Demonstração de múltiplas instâncias

Com Docker Desktop e PowerShell, execute a partir da raiz do repositório:

```powershell
pwsh -File .\scripts\demo-multiple-instances.ps1
```

O script sobe o ambiente, aplica migrations, espera as três APIs e executa duas provas reais por
HTTP:

1. Envia duas apostas diferentes de `80.00` para `api-a` e `api-b`, ao mesmo tempo, sobre uma
   carteira com `100.00`. Consulta o resultado por `api-c`: uma aposta é processada, a outra é
   rejeitada, o saldo termina em `20.00` e há apenas um débito no ledger.
2. Envia uma aposta de `25.00` para cada uma de três carteiras diferentes, em paralelo, por
   `api-a`, `api-b` e `api-c`. As três devem ser processadas e cada carteira termina em `75.00`.
3. Envia 50 cópias da mesma aposta de `25.00`, em rodízio entre `api-a`, `api-b` e `api-c`.
   Confirma saldo final de `75.00`, um único débito e 49 replays idempotentes.

Ao terminar, os containers continuam ativos para inspeção. Para encerrá-los:

```powershell
docker compose down
```

## 6. Evidências automatizadas

```powershell
make verify
```

Esse comando roda testes unitários, detector de corrida, análise estática e integração com
PostgreSQL/LocalStack reais. A integração cobre disputa de saldo, 50 replays paralelos,
reentrega SQS após commit antes da remoção da mensagem, referências pendentes/expiradas e três
publishers concorrentes da outbox.

## Limitações conhecidas e evolução de produção

- A métrica `wallet_reconciliation_divergences_total` é exposta em `GET /metrics`; métricas
  operacionais mais amplas e tracing distribuído permanecem como evolução de produção.
- A entrega de eventos é at-least-once. Consumidores de `wager-events.fifo` devem deduplicar por
  `eventId`, que permanece estável em republicações.
- Este ambiente local usa três portas para tornar cada instância visível ao roteiro. Um
  balanceador de carga e autoscaling não são requisitos do desafio e não foram implementados.
  Caso o sistema evolua para produção, um orquestrador (por exemplo, Kubernetes ou ECS) e um
  balanceador poderiam manter e distribuir instâncias conforme a demanda, sem alterar a regra
  financeira que permanece no PostgreSQL.
