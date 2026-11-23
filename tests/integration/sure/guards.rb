# Real Sure model checks, rolled back after each case. No fake persistence layer.
source = SimplefinAccount.find_by!(account_id: 'degitide-default-EUR')
account = source.current_account
raise 'adapter is not installed' unless defined?(DegitideSimplefin)

def check(value, message)
  raise message unless value
end

def set_snapshot_balance(payload, value)
  payload['balance'] = value
  day = Time.iso8601(payload.dig('extra', 'degitide', 'snapshot_at')).to_date.iso8601
  Array(payload.dig('extra', 'degitide', 'balance_history')).each do |row|
    row['balance'] = value if row['date'] == day
  end
end

ActiveRecord::Base.transaction do
  original = account.current_holdings.first!
  original_security = original.provider_security || original.security
  manual_security = Security.create!(ticker: 'E2E_MANUAL_GUARD', name: 'Manual guard')
  manual = account.holdings.create!(security: manual_security, date: Date.yesterday,
    qty: 7, price: 10, amount: 70, currency: 'EUR', external_id: 'e2e-manual')
  history = original.dup
  # Sure may already have reconstructed yesterday's holding from the trades.
  # Use an unused earlier day so this guard tests preservation, not uniqueness.
  history.assign_attributes(date: [Date.yesterday, account.holdings.minimum(:date) - 1].min,
    external_id: 'e2e-history')
  history.save!
  history_values = history.attributes.except('updated_at')

  remapped = Security.create!(ticker: 'E2E_REMAP_GUARD', name: 'User remapping')
  original.update!(security: remapped, security_locked: true,
    provider_security: original_security, cost_basis: 99, cost_basis_locked: true,
    cost_basis_source: 'manual')
  SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
  check(original.reload.qty.positive?, 'open remapped position was closed')
  check(original.security_id == remapped.id && original.cost_basis == 99, 'user corrections were overwritten')

  complete = source.raw_payload.deep_dup
  set_snapshot_balance(complete, '0')
  complete['holdings'] = []
  source.update!(raw_payload: complete, raw_holdings_payload: [])
  SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
  check(original.reload.qty.zero?, 'remapped closed position survived')
  check(original.security_id == remapped.id && original.cost_basis == 99, 'closing lost user corrections')
  check(manual.reload.qty == 7, 'manual holding changed')
  check(history.reload.attributes.except('updated_at') == history_values, 'history was rewritten')
  raise ActiveRecord::Rollback
end

[:original, :remapped].each do |kind|
  ActiveRecord::Base.transaction do
    source.reload
    history = account.current_holdings.first!.dup
    account.holdings.delete_all
    if kind == :remapped
      history.provider_security ||= history.security
      history.security = Security.create!(ticker: 'E2E_MANUAL_COLLISION', name: 'Remapped collision')
      history.security_locked = true
    end
    history.date = Date.yesterday
    history.save!
    manual = account.holdings.create!(security: history.security, date: Date.current,
      currency: history.currency, qty: 7, amount: 70, price: 10)
    before = [history.attributes, manual.attributes]
    payload = source.raw_payload.deep_dup
    payload['holdings'] = []
    set_snapshot_balance(payload, '0')
    source.update!(raw_payload: payload, raw_holdings_payload: [])
    SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
    check([history.reload.attributes, manual.reload.attributes] == before, "#{kind} collision overwrote holdings")
    check(account.current_holdings.empty?, "#{kind} collision retained a closed provider position")
    payload['extra']['degitide']['snapshot_at'] = 2.days.ago.iso8601
    source.update!(raw_payload: payload)
    check(account.current_holdings.empty?, 'an aging accepted snapshot reopened a closed position')
    raise ActiveRecord::Rollback
  end
end

ActiveRecord::Base.transaction do
  source.reload
  closed = account.current_holdings.first!.dup
  account.holdings.delete_all
  closed.assign_attributes(date: Date.yesterday, qty: 0, amount: 0, price: 0)
  closed.save!
  payload = source.raw_payload.deep_dup
  payload['holdings'] = []
  set_snapshot_balance(payload, '0')
  source.update!(raw_payload: payload, raw_holdings_payload: [])
  SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
  check(account.holdings.count == 1, 'already closed position gained a redundant zero row')
  raise ActiveRecord::Rollback
end

[:incomplete, :missing, :stale, :future, :invalid, :other_provider].each do |kind|
  ActiveRecord::Base.transaction do
    source.reload
    before = account.holdings.order(:id).map(&:attributes)
    before_valuations = account.entries.valuations.order(:id).map(&:attributes)
    payload = source.raw_payload.deep_dup
    payload['holdings'] = []
    set_snapshot_balance(payload, '0')
    case kind
    when :incomplete then payload['extra']['degitide']['complete_holdings'] = false
    when :stale then payload['extra']['degitide']['snapshot_at'] = 2.days.ago.iso8601
    when :future then payload['extra']['degitide']['snapshot_at'] = 2.days.from_now.iso8601
    when :invalid then payload['balance'] = 'NaN'
    when :other_provider then payload['org']['id'] = 'other-provider'
    when :missing
      response = source.simplefin_item.raw_payload.deep_dup
      response['accounts'].reject! { |a| a['id'] == source.account_id }
      source.simplefin_item.update!(raw_payload: response)
    end
    source.update!(raw_payload: payload, raw_holdings_payload: [])
    SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
    check(account.holdings.order(:id).map(&:attributes) == before, "#{kind} snapshot changed holdings")
    check(account.entries.valuations.order(:id).map(&:attributes) == before_valuations, "#{kind} snapshot changed valuations")
    raise ActiveRecord::Rollback
  end
end
puts 'PASS adapter guards: history, manual collisions, user remapping/cost locks, closed-row idempotency, incomplete/missing/stale/future/invalid/other-provider snapshots'


ActiveRecord::Base.transaction do
  source.reload
  entry = account.entries.valuations.find_by!(source: DegitideSimplefin::HISTORY_SOURCE)
  entry.update!(amount: 777, user_modified: true)
  before = entry.attributes
  payload = source.raw_payload.deep_dup
  row = payload.dig('extra', 'degitide', 'balance_history').find { |r| r['date'] == entry.date.iso8601 }
  row['balance'] = '888'
  source.update!(raw_payload: payload)
  SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
  check(entry.reload.attributes == before, 'user-modified historical valuation overwritten')
  raise ActiveRecord::Rollback
end

ActiveRecord::Base.transaction do
  source.reload
  day = Date.current - 3
  # The weekly fixtures are Friday; use an unused date for a manual valuation.
  day -= 1 while account.entries.valuations.where(date: day).exists?
  manual = account.entries.create!(date: day, name: 'Manual reconciliation', amount: 123,
    currency: account.currency, entryable: Valuation.new(kind: 'reconciliation'))
  before = manual.attributes
  payload = source.raw_payload.deep_dup
  payload.dig('extra', 'degitide', 'balance_history') << {'date'=>day.iso8601,'balance'=>'456','source'=>'report'}
  payload['extra']['degitide']['balance_history'].sort_by! { |r| r['date'] }
  source.update!(raw_payload: payload)
  SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
  check(manual.reload.attributes == before, 'manual valuation/date collision overwritten')
  raise ActiveRecord::Rollback
end

[:user_modified, :locked, :manually_dated, :previously_updated].each do |kind|
  ActiveRecord::Base.transaction do
    source.reload
    account.entries.valuations.where(source: DegitideSimplefin::HISTORY_SOURCE).destroy_all
    attributes = {date: account.created_at.to_date.advance(years: -2),
      name: Valuation.build_opening_anchor_name(account.accountable_type), amount: 999,
      currency: account.currency, created_at: account.created_at, updated_at: account.created_at,
      entryable: Valuation.new(kind: 'opening_anchor')}
    attributes[:user_modified] = true if kind == :user_modified
    attributes[:locked_attributes] = {'amount'=>true} if kind == :locked
    attributes[:date] -= 1 if kind == :manually_dated
    attributes[:updated_at] += 1.hour if kind == :previously_updated
    anchor = account.entries.create!(attributes)
    before = anchor.attributes
    SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
    check(anchor.reload.attributes == before, "#{kind} opening anchor rewritten")
    raise ActiveRecord::Rollback
  end
end

ActiveRecord::Base.transaction do
  source.reload
  first = account.entries.valuations.where(source: DegitideSimplefin::HISTORY_SOURCE).order(:date).first!
  previous_date, previous_amount = first.date, first.amount
  earlier = previous_date - 7
  payload = source.raw_payload.deep_dup
  payload['extra']['degitide']['balance_history'].unshift({'date'=>earlier.iso8601,'balance'=>'0','source'=>'report'})
  source.update!(raw_payload: payload)
  SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
  check(first.reload.date == earlier && first.amount.zero?, 'older zero observation did not extend the owned opening')
  preserved = account.entries.valuations.find_by!(date: previous_date)
  check(preserved.amount == previous_amount && preserved.entryable.reconciliation?, 'extending history lost the former first observation')
  raise ActiveRecord::Rollback
end

# The feed's snapshot day is the timestamp's local calendar date. Exercise both
# sides of UTC midnight: accepting same-day data and excluding it from history
# import must use that same boundary, regardless of the instant's UTC date.
['2025-07-01T00:30:00+01:00', '2025-07-01T23:30:00-01:00'].each do |timestamp|
  ActiveRecord::Base.transaction do
    source.reload
    account.entries.valuations.destroy_all
    payload = source.raw_payload.deep_dup
    marker = payload['extra']['degitide']
    marker['snapshot_at'] = timestamp
    marker['balance_history'] = [
      {'date'=>'2025-06-30', 'balance'=>'123', 'source'=>'report'},
      {'date'=>'2025-07-01', 'balance'=>payload['balance'], 'source'=>'snapshot'}
    ]
    check(DegitideSimplefin.complete?(payload, check_time: false), "offset snapshot rejected: #{timestamp}")
    source.update!(raw_payload: payload)
    DegitideSimplefin::ValuationsProcessor.new(source.reload).process
    imported = account.entries.valuations.where(source: DegitideSimplefin::HISTORY_SOURCE)
    check(imported.pluck(:date, :amount) == [[Date.new(2025, 6, 30), 123.to_d]],
      "offset snapshot used the wrong historical boundary: #{timestamp}")
    raise ActiveRecord::Rollback
  end
end

[:duplicate, :unordered, :nonfinite, :future, :currency, :scope, :version, :current_mismatch].each do |kind|
  ActiveRecord::Base.transaction do
    source.reload
    before = account.entries.valuations.order(:id).map(&:attributes)
    payload = source.raw_payload.deep_dup
    marker = payload['extra']['degitide']
    rows = marker['balance_history']
    case kind
    when :duplicate then rows.insert(1, rows.first.deep_dup)
    when :unordered then rows.reverse!
    when :nonfinite then rows.first['balance'] = 'NaN'
    when :future then rows.last['date'] = Date.tomorrow.iso8601
    when :currency then rows.first['currency'] = 'USD'
    when :scope then marker['history_scope'] = 'cash'
    when :version then marker['history_version'] = 99
    when :current_mismatch then rows.last['balance'] = '987654'
    end
    check(!DegitideSimplefin.complete?(payload), "#{kind} history accepted")
    source.update!(raw_payload: payload)
    SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
    check(account.entries.valuations.order(:id).map(&:attributes) == before, "#{kind} history mutated valuations")
    raise ActiveRecord::Rollback
  end
end

ActiveRecord::Base.transaction do
  source.reload
  before = account.entries.valuations.order(:id).map(&:attributes)
  payload = source.raw_payload.deep_dup
  payload['extra']['degitide']['balance_history'] = []
  source.update!(raw_payload: payload)
  SimplefinAccount::Investments::HoldingsProcessor.new(source.reload).process
  check(account.entries.valuations.order(:id).map(&:attributes) == before, 'empty history deleted prior observations')
  raise ActiveRecord::Rollback
end
source.reload
first_history = account.entries.valuations.where(source: DegitideSimplefin::HISTORY_SOURCE).minimum(:date)
combined_start = Balance::LinkedInvestmentSeriesNormalizer.send(:common_supported_history_start_date, [account.id])
check(combined_start <= first_history, 'aggregate net-worth chart excludes valuation history')
puts 'PASS history guards: native valuations, manual collisions, edited/locked openings, offset midnight boundaries, invalid history, empty-history preservation and aggregate chart coverage'
