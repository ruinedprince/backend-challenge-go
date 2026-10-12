# Arquitetura

## Limite de responsabilidades

`cmd/api` apenas inicia o processo. `internal/app` é a composição de infraestrutura
por Uber Fx; regras financeiras não importam HTTP, SQS, PostgreSQL nem Fx.

## Inicialização e encerramento

Fx cria a configuração, o pool PostgreSQL, o cliente SQS, a checagem de prontidão e
o servidor HTTP. Antes de expor HTTP, a aplicação confirma `Ping` no PostgreSQL e
uma chamada ao SQS. O servidor abre o socket no próprio `OnStart`, portanto falha de
porta ocupada impede que a aplicação seja declarada iniciada.

No encerramento, o HTTP interrompe novas conexões e respeita
`APP_SHUTDOWN_TIMEOUT`; consumidores, publisher e resolvedor de referências cancelam novos
polls e aguardam o trabalho em andamento. Só então o pool PostgreSQL é fechado.

## Ambiente local

O Compose usa PostgreSQL, Keycloak e LocalStack. O script do LocalStack cria
`wager-transactions.fifo`, sua DLQ com redrive após cinco recebimentos e a fila de saída
`wager-events.fifo`. O realm do Keycloak é importado automaticamente na inicialização.

Para demonstrar a exigência de concorrência, o Compose também inicia `api-a`, `api-b` e `api-c`.
São três processos idênticos da aplicação, cada qual com memória e pool de conexões próprios,
compartilhando os mesmos serviços de infraestrutura. As portas locais `8080`, `8082` e `8083`
permitem que o roteiro automatizado direcione chamadas para uma instância específica. Isso não é
um balanceador de carga nem autoscaling: ambos estão fora do escopo do desafio e seriam uma
responsabilidade da infraestrutura em uma implantação de produção.

## Migrations

As migrations são aplicadas pelo container `migrate` via `make migrate-up` e revertidas
uma a uma com `make migrate-down`. `000001_bootstrap` é propositalmente neutra: ela
valida a cadeia de migração antes que `000002_financial_schema` introduza o schema
financeiro. `000004_pending_references` adiciona a agenda durável das reversões pendentes.
`make schema-test` executa um roteiro SQL contra o PostgreSQL real para confirmar a
unicidade de carteira, a imutabilidade do ledger e a fórmula do saldo.

## Domínio financeiro

`internal/domain` não depende de infraestrutura. `Money` armazena valores em `int64`
de unidades mínimas (centavos), junto do código ISO 4217; portanto nenhum cálculo,
parsing ou JSON usa ponto flutuante. A entrada aceita somente decimal simples com até
duas casas e serializa de volta na forma canônica `"25.00"`. O limite representável é
de -92.233.720.368.547.758,08 a 92.233.720.368.547.758,07 por moeda.

`Wallet` é a raiz do agregado: começa na versão 1, rejeita saldo negativo e só muda por
`Debit` ou `Credit`. Cada uma dessas operações cria um `WalletLedgerEntry` imutável que
valida o saldo anterior e posterior. A atualização da carteira e a inserção desse lançamento
ocorrem na mesma transação SQL, usando a versão para impedir que uma escrita concorrente
descarte outra.

`WagerTransaction` contém a máquina de estados `PENDING` → `PENDING_REFERENCE` ou um
estado terminal (`PROCESSED`, `REJECTED`, `FAILED`). Tipos externos validam seus valores
antes de qualquer I/O: `LOSS` exige zero; `BET`, `WIN`, `REFUND` e `ROLLBACK` exigem
valor positivo; `OPENING` é reservado à criação interna de carteira. Reversões exigem
uma referência externa; a validação da transação referenciada entra no caso de uso do
BCG-5, quando houver persistência para consultá-la.

## Schema financeiro

`wallets` mantém saldo em unidades mínimas (`BIGINT`) e versão. A constraint de saldo
não negativo e a unicidade `(player_id, currency)` são garantias do banco, não somente
da aplicação. `wager_transactions` separa a abertura interna de operações externas,
guarda idempotência e impõe valor zero em `LOSS`, valor positivo nos demais tipos
externos e referência obrigatória em `REFUND`/`ROLLBACK`. Índices parciais impedem a
reaplicação da mesma identidade externa e duas reversões bem-sucedidas do mesmo tipo.

`wallet_ledger_entries` valida a aritmética de cada lançamento, é único por
`(wallet_id, transaction_id)` e possui triggers que rejeitam `UPDATE` e `DELETE`; ele é
append-only. Inbox e outbox já estão persistidos no schema: a primeira deduplica por
`(consumer_name, message_id)` e a segunda registra eventos pendentes com tentativas e
agendamento de retry. O caso de uso grava carteira, transação, ledger e outbox na mesma
transação SQL.

## Processamento financeiro e concorrência

`internal/core.Processor` é o caso de uso compartilhado por futuras entradas HTTP e
SQS. Para cada pedido, ele gera SHA-256 de uma estrutura JSON canônica com os campos de
negócio (sem chave de idempotência nem metadados de transporte), abre uma transação SQL e
faz `SELECT ... FOR UPDATE` apenas na carteira solicitada. Assim, duas carteiras
independentes não se bloqueiam, enquanto débitos concorrentes da mesma carteira avançam
em série.

A mesma transação SQL consulta os replays persistidos, cria a transação, atualiza saldo e
versão, adiciona o ledger e grava eventos na outbox. Se uma aposta não tiver saldo, ela é
registrada como `REJECTED` com `INSUFFICIENT_FUNDS`, sem ledger ou alteração de saldo. Um
reenvio com chave e hash iguais devolve exatamente o saldo/resultados gravados; uma mesma
chave com conteúdo diferente é conflito. Além do lock, o `UPDATE` usa a versão anterior
na cláusula `WHERE` como proteção contra atualização perdida.

O fluxo síncrono suporta `BET`, `WIN` e `LOSS`. `REFUND` e `ROLLBACK` resolvem a
referência persistida e usam a mesma transação SQL para saldo, ledger e estado. O teste de integração executa duas
`BET` concorrentes de 80.00 contra saldo 100.00: uma é processada, a outra rejeitada, o
saldo final é 20.00 e há somente um débito no ledger.

## Referências pendentes e reversões

Uma reversão recebida antes da operação de origem é registrada como `PENDING_REFERENCE`,
gera seu evento de pendência e recebe tentativa/agendamento duráveis. O resolvedor busca
pendências a cada segundo; após cada referência ainda indisponível aplica backoff exponencial.
No quinto retry sem referência, a operação termina como `REJECTED` com
`REFERENCE_NOT_FOUND`. A reinicialização é segura porque a agenda está no PostgreSQL.

Referência e reversão precisam ter o mesmo provedor, jogador, carteira, rodada, moeda e valor.
`REFUND` só devolve uma `BET`; `ROLLBACK` desfaz `BET`, `WIN` ou `REFUND`. Para uma aposta,
`REFUND` e `ROLLBACK` são mutuamente exclusivos: uma segunda tentativa recebe
`REVERSAL_ALREADY_APPLIED`. Outros códigos estáveis de rejeição são `REFERENCE_MISMATCH`,
`REFERENCE_TYPE_INVALID`, `REFERENCE_NOT_PROCESSABLE` e
`INSUFFICIENT_FUNDS_FOR_REVERSAL`.

## HTTP, OAuth/OIDC e isolamento por provedor

O Keycloak emite tokens de client credentials para cada provedor. A API descobre as chaves
públicas no issuer OIDC configurado, verifica assinatura, issuer e expiração do bearer token
e extrai o cliente autorizado da claim assinada `azp`. O `providerId` enviado no corpo ou
na rota precisa ser igual ao `azp`; portanto ele não é confiado apenas porque veio no JSON.

`POST /wallets`, leituras de carteira/ledger/reconciliação e os endpoints de transação exigem
bearer token. A tabela `wallet_provider_access` vincula uma nova carteira ao provedor que a
abriu e o núcleo financeiro verifica esse vínculo dentro da própria transação antes de mexer
no saldo. Assim, conhecer um UUID de carteira não concede acesso a outro provedor. A abertura
positiva registra `OPENING`, crédito no ledger e dois eventos de outbox no mesmo commit; uma
abertura com saldo zero não cria movimento financeiro.
## Consumo SQS e inbox

O consumidor faz long polling de `wager-transactions.fifo`. Para cada envelope, calcula o
hash do corpo, grava a inbox e executa o mesmo caso de uso financeiro na mesma transação SQL.
A mensagem só é removida da SQS após o commit; falhas ficam invisíveis por 30 segundos e são
tentadas novamente, chegando à DLQ após cinco recebimentos. Reentrega com o mesmo `messageId`
é um no-op; o mesmo ID com hash diferente é conflito auditável. No encerramento, o worker para
de buscar mensagens e aguarda o polling ativo terminar.

## Publicação pela outbox

Cada evento recebe um snapshot JSON tipado ainda dentro da transação financeira: o evento da
transação contém sua identidade e estado; `WalletBalanceChanged` também contém direção, valor,
saldo antes/depois e versão resultante da carteira. Um publisher separado reivindica lotes com
`FOR UPDATE SKIP LOCKED`, portanto duas instâncias não enviam o mesmo registro enquanto ambas
estão saudáveis. A reivindicação expira após 30 segundos: se a instância cair, outra pode assumir
o trabalho. Falhas de envio liberam o registro com backoff exponencial, limitado a cinco minutos.

O destino é `wager-events.fifo`. O envelope transportado contém `eventId`, tipo, agregado,
correlação, instante UTC, versão e dados. `MessageGroupId` é o agregado e
`MessageDeduplicationId` é o próprio `eventId`. Se a aplicação cair depois de enviar à SQS e antes
de registrar `published_at`, ela republica o mesmo `eventId`; consumidores devem deduplicá-lo.
