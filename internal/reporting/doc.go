// Package reporting owns read-only finance aggregates for the merchant: the BD7 daily captured / refunded /
// net summary by currency and environment over at most 92 days in the Asia/Taipei day (Q11), and its CSV
// export (orders:export, audited).
//
// It never writes: no ledger, no fact, no refund state, no second ledger and no cache. It never converts
// currency (a sum is always within one currency and environment, I05) and never reads any table itself; the
// money rules live in identity.read_finance_summary / export_finance_summary (migrations/0078), which derive
// the rows from payments.facts and payments.refund_facts. Profit/COGS reporting is deferred.
package reporting
