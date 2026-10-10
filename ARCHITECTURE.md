# Arquitetura

## Limite de responsabilidades

`cmd/api` apenas inicia o processo. `internal/app` é a composição de infraestrutura
por Uber Fx; regras financeiras futuras não importam HTTP, SQS, PostgreSQL nem Fx.
Os próximos cards introduzirão domínio, repositórios e workers atrás desse limite.

## Inicialização e encerramento

Fx cria a configuração, o pool PostgreSQL, o cliente SQS, a checagem de prontidão e
o servidor HTTP. Antes de expor HTTP, a aplicação confirma `Ping` no PostgreSQL e
uma chamada ao SQS. O servidor abre o socket no próprio `OnStart`, portanto falha de
porta ocupada impede que a aplicação seja declarada iniciada.

No encerramento, o HTTP interrompe novas conexões e respeita
`APP_SHUTDOWN_TIMEOUT`; só então o pool PostgreSQL é fechado. Workers entrarão nesse
mesmo ciclo nos cards de SQS e outbox.

## Ambiente local

O Compose usa PostgreSQL, Keycloak e LocalStack. O script do LocalStack cria
`wager-transactions.fifo` e sua DLQ com redrive após cinco recebimentos. A integração
OIDC e os contratos das filas serão detalhados junto dos respectivos adaptadores.

## Migrations

As migrations são aplicadas pelo container `migrate` via `make migrate-up` e revertidas
uma a uma com `make migrate-down`. `000001_bootstrap` é propositalmente neutra: ela
valida a cadeia de migração antes que `000002_financial_schema` introduza o schema
financeiro. `make schema-test` executa um roteiro SQL contra o PostgreSQL real para
confirmar a unicidade de carteira, a imutabilidade do ledger e a fórmula do saldo.

## Domínio financeiro

`internal/domain` não depende de infraestrutura. `Money` armazena valores em `int64`
de unidades mínimas (centavos), junto do código ISO 4217; portanto nenhum cálculo,
parsing ou JSON usa ponto flutuante. A entrada aceita somente decimal simples com até
duas casas e serializa de volta na forma canônica `"25.00"`. O limite representável é
de -92.233.720.368.547.758,08 a 92.233.720.368.547.758,07 por moeda.

`Wallet` é a raiz do agregado: começa na versão 1, rejeita saldo negativo e só muda por
`Debit` ou `Credit`. Cada uma dessas operações cria um `WalletLedgerEntry` imutável que
valida o saldo anterior e posterior. No BCG-4/5, a atualização da carteira e a inserção
desse lançamento ocorrerão na mesma transação SQL, usando a versão para impedir que uma
escrita concorrente descarte outra.

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
agendamento de retry. O caso de uso do BCG-5 gravará carteira, transação, ledger e
outbox na mesma transação SQL.

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

No BCG-5, o fluxo síncrono suporta `BET`, `WIN` e `LOSS`. `REFUND` e `ROLLBACK` exigem a
resolução persistida da referência e entram no BCG-9. O teste de integração executa duas
`BET` concorrentes de 80.00 contra saldo 100.00: uma é processada, a outra rejeitada, o
saldo final é 20.00 e há somente um débito no ledger.
