# Read actual persisted Sure state, without secrets or access URLs.
require 'json'
result = {
  accounts: SimplefinAccount.order(:account_id).map do |sfa|
    account = sfa.current_account
    {
      source_id: sfa.account_id, provider_id: sfa.id, account_id: account&.id,
      balance: account&.balance&.to_s, cash: account&.cash_balance&.to_s,
      raw_holdings: Array(sfa.raw_holdings_payload).size,
      history: Array(sfa.raw_payload&.dig('extra', 'degitide', 'balance_history')),
      history_start: account&.history_start_date,
      valuations: account ? account.entries.valuations.order(:date).map { |e| {id:e.id,date:e.date,amount:e.amount.to_s,source:e.source,external_id:e.external_id,kind:e.entryable.kind} } : [],
      historical_balances: account ? account.balances.where(currency: account.currency).where(date: Array(sfa.raw_payload&.dig('extra', 'degitide', 'balance_history')).map { |p| p['date'] }).order(:date).map { |b| {date:b.date,amount:b.end_balance.to_s} } : [],
      holdings: account ? account.holdings.order(:date, :security_id).map { |h| {id: h.id, date: h.date, qty: h.qty.to_s, amount: h.amount.to_s, ticker: h.security.ticker, provider: h.account_provider_id} } : [],
      current: account ? account.current_holdings.map { |h| {id:h.id, qty:h.qty.to_s, amount:h.amount.to_s, ticker:h.security.ticker} } : [],
      entries: account ? account.entries.order(:external_id).map { |e| {id: e.id, external_id: e.external_id, type: e.entryable_type, amount: e.amount.to_s} } : []
    }
  end,
  syncs: Sync.order(:created_at).map { |s| {id: s.id, type: s.syncable_type, status: s.status, error: s.error, stats:s.sync_stats} },
  items: SimplefinItem.order(:created_at).map { |s| {id:s.id, pending:s.pending_account_setup, status:s.status, last_synced_at:s.last_synced_at} }
}
puts "E2E_JSON:" + JSON.generate(result)
