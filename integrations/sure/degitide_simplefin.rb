# frozen_string_literal: true
# Compatibility adapter for Sure v0.7.6. Mount this initializer in BOTH Sure
# web and worker containers. Only explicitly marked Degitide snapshots opt in.
module DegitideSimplefin
  VERSION = 2
  PAYLOAD_VERSION = 1
  HISTORY_SOURCE = "degitide_valuation"
  HISTORY_NAME = "DEGIRO recorded holdings value"
  SYMBOL = /\ACUSTOM:DEGIRO_[0-9A-F]{32}\z/

  def self.marked?(payload)
    payload.is_a?(Hash) && payload.dig("extra", "degitide", "version") == PAYLOAD_VERSION
  end

  def self.complete?(payload, check_time: true, check_history: true)
    return false unless marked?(payload)
    marker = payload.dig("extra", "degitide")
    return false unless marker["complete_holdings"] == true
    return false unless payload["id"].to_s.start_with?("degitide-")
    return false unless payload.dig("org", "id") == "degitide"
    return false unless payload["holdings"].is_a?(Array)
    return false unless payload["currency"].to_s.match?(/\A[A-Z]{3}\z/)

    snapshot_time = Time.iso8601(marker.fetch("snapshot_at"))
    return false if check_time && !snapshot_time.between?(36.hours.ago, 1.minute.from_now)
    symbols = []
    total = 0.to_d
    payload["holdings"].each do |row|
      return false unless row.is_a?(Hash) && row["symbol"].to_s.match?(SYMBOL)
      return false unless row["id"].to_s.start_with?("degitide-") && row["currency"] == payload["currency"]
      qty = BigDecimal(row.fetch("shares").to_s)
      value = BigDecimal(row.fetch("market_value").to_s)
      return false unless qty.finite? && qty.positive? && value.finite? && value >= 0
      return false if symbols.include?(row["symbol"])
      symbols << row["symbol"]
      total += value
    end
    historical_rows(payload) if check_history # Validate the optional extension before any writes.
    balance = BigDecimal(payload.fetch("balance").to_s)
    balance.finite? && (total - balance).abs <= BigDecimal("0.00000001")
  rescue ArgumentError, KeyError, TypeError
    false
  end

  # This is an explicit Degitide extension, not part of the SimpleFIN standard.
  # Missing history is valid for older feeds; malformed advertised history is not.
  def self.historical_rows(payload)
    marker = payload.dig("extra", "degitide")
    return nil unless marker.is_a?(Hash) && marker.key?("balance_history")
    raise ArgumentError, "Invalid Degitide history format" unless
      marker["history_version"] == 1 && marker["history_scope"] == "securities_only" &&
      marker["balance_history"].is_a?(Array) && marker["balance_history"].size <= 20_000

    # Go's saved snapshot date uses this timestamp's calendar date, including
    # its offset. Converting to UTC would shift observations around midnight.
    latest = Time.iso8601(marker.fetch("snapshot_at")).to_date
    previous = nil
    marker["balance_history"].map do |row|
      raise ArgumentError, "Invalid Degitide history row" unless row.is_a?(Hash)
      day = Date.iso8601(row.fetch("date"))
      amount = BigDecimal(row.fetch("balance").to_s)
      raise ArgumentError, "Invalid Degitide history value" unless
        day.iso8601 == row["date"] && day > Entry.min_supported_date && day <= latest &&
        (previous.nil? || day > previous) && amount.finite? && amount >= 0 && amount < 10**15 &&
        %w[report snapshot].include?(row["source"]) &&
        (!row.key?("currency") || row["currency"] == payload["currency"])
      if day == latest && (amount - BigDecimal(payload.fetch("balance").to_s)).abs > BigDecimal("0.00000001")
        raise ArgumentError, "Degitide history disagrees with current snapshot"
      end
      previous = day
      { date: day, balance: amount, source: row["source"] }
    end
  end

  def self.protected_entry?(entry)
    entry.protected_from_sync? || entry.locked_attributes.present? || entry.excluded? ||
      entry.reconciled_at.present? ||
      (entry.entryable.respond_to?(:locked_attributes) && entry.entryable.locked_attributes.present?)
  end

  class ValuationsProcessor
    def initialize(simplefin_account)
      @source = simplefin_account
      @account = simplefin_account.current_account
    end

    # Caller holds the SimplefinAccount row lock and checked that this account
    # was present in the current response. Serialize valuation/date collisions
    # with manual edits and other provider processors under the account lock.
    def process
      payload = @source.raw_payload
      rows = DegitideSimplefin.historical_rows(payload)
      return if rows.blank? || @account&.accountable_type != "Investment"
      return unless @account.currency == payload["currency"]
      latest = Time.iso8601(payload.dig("extra", "degitide", "snapshot_at")).to_date
      rows = rows.select { |row| row[:date] < latest }
      return if rows.empty?
      @prefix = "degitide-history:#{payload.fetch('id')}:"

      @account.with_lock do
        adopt_opening_anchor(rows.first)
        rows.each do |row|
          external_id = @prefix + row[:date].iso8601
          entry = @account.entries.valuations.find_by(source: HISTORY_SOURCE, external_id: external_id)
          next if entry && DegitideSimplefin.protected_entry?(entry)
          occupied = @account.entries.valuations.find_by(date: row[:date])
          next if occupied && occupied.id != entry&.id
          entry ||= @account.entries.build(source: HISTORY_SOURCE, external_id: external_id,
            entryable: Valuation.new(kind: "reconciliation"))
          entry.assign_attributes(date: row[:date], amount: row[:balance], currency: payload["currency"],
            name: HISTORY_NAME, notes: note(row))
          entry.save! if entry.new_record? || entry.changed?
        end
      end
    end

    private

    def note(row)
      detail = row[:source] == "report" ? "DEGIRO closing position report" : "Degitide saved holdings snapshot"
      "#{detail}. Securities only; excludes cash. This is an observed valuation, not a deposit or spending transaction."
    end

    def adopt_opening_anchor(row)
      entry = @account.valuations.opening_anchor.includes(:entry).first&.entry
      return unless entry && !DegitideSimplefin.protected_entry?(entry)
      owned = entry.source == HISTORY_SOURCE && entry.external_id.to_s.start_with?(@prefix)
      # Repair only Sure's untouched automatic two-year opening. A changed,
      # imported, manually dated or otherwise owned anchor remains the user's.
      automatic = entry.source.blank? && entry.external_id.blank? && entry.import_id.nil? &&
        @account.simplefin_account_id == @source.id &&
        entry.name == Valuation.build_opening_anchor_name(@account.accountable_type) &&
        entry.date == @account.created_at.to_date.advance(years: -2) &&
        entry.created_at.between?(@account.created_at, @account.created_at + 1.minute) &&
        entry.updated_at == entry.created_at
      return unless owned || automatic
      return if owned && entry.date < row[:date]
      occupied = @account.entries.valuations.where(date: row[:date]).where.not(id: entry.id).exists?
      return if occupied
      # Don't move the boundary past existing activity or a manual valuation.
      return if @account.entries.where.not(id: entry.id).where("date < ?", row[:date]).exists?
      entry.assign_attributes(date: row[:date], amount: row[:balance], currency: @account.currency,
        source: HISTORY_SOURCE, external_id: @prefix + row[:date].iso8601, name: HISTORY_NAME, notes: note(row))
      entry.save! if entry.changed?
    end
  end

  # Sure v0.7.6 excludes all valuations when deciding where linked investment
  # charts begin. Our explicitly sourced historical observations are supported
  # history too, even while trade activity is disabled.
  module SupportedHistory
    def supported_history_start_date
      [super, account.entries.valuations.where(source: HISTORY_SOURCE, excluded: false).minimum(:date)].compact.min
    end
  end

  module AggregateSupportedHistory
    private

    def stable_provider_holding_start_dates(account_ids)
      super.merge(Entry.valuations.where(account_id: account_ids, source: HISTORY_SOURCE, excluded: false)
        .group(:account_id).minimum(:date)) { |_id, holdings_date, history_date| [holdings_date, history_date].compact.min }
    end
  end

  module Importer
    private

    def import_account(account_data)
      payload = account_data.deep_stringify_keys
      return super unless DegitideSimplefin.marked?(payload)
      raise ArgumentError, "Invalid complete Degitide snapshot" unless DegitideSimplefin.complete?(payload)

      # Upstream ignores holdings: [] and can replace raw_payload with an older
      # payload when holdings change. Save the full authoritative snapshot in
      # the same transaction, before asynchronous jobs can read it.
      SimplefinAccount.transaction do
        result = super
        source = simplefin_item.simplefin_accounts.find_by!(account_id: payload.fetch("id"))
        unless source.current_balance == BigDecimal(payload.fetch("balance").to_s)
          raise "Sure did not accept the Degitide balance"
        end
        source.update!(raw_payload: payload, extra: payload["extra"], raw_holdings_payload: payload["holdings"])
        result
      end
    end
  end

  module HoldingsProcessor
    def process
      return super unless DegitideSimplefin.marked?(simplefin_account.raw_payload)

      # Jobs and full syncs can overlap. Reload under a provider-row lock so an
      # older queued job cannot restore a position from its in-memory snapshot.
      simplefin_account.with_lock do
        payload = simplefin_account.raw_payload
        return unless DegitideSimplefin.complete?(payload)
        # A partial feed error omits the failed account. Keep its prior state;
        # absence from an errored response is never evidence of a sale.
        current_response = simplefin_account.simplefin_item.reload.raw_payload
        current_ids = Array(current_response&.dig("accounts")).map { |a| a["id"] }
        return unless current_ids.include?(payload["id"])
        return unless account&.accountable_type == "Investment"
        provider = simplefin_account.account_provider
        return unless provider

        if simplefin_account.raw_holdings_payload != payload["holdings"]
          simplefin_account.update!(raw_holdings_payload: payload["holdings"])
        end
        super

        open_symbols = payload["holdings"].map { |row| row.fetch("symbol") }
        snapshot_date = Date.current # Matches Sure's SimpleFIN processor.
        # Select the newest row for each source instrument/currency. Preserve
        # history and user remappings; never claim manual or other-provider rows.
        previous = account.holdings
          .where(account_provider_id: provider.id, currency: payload["currency"])
          .where("date <= ?", snapshot_date)
          .includes(:security, :provider_security)
          .order(date: :desc, id: :asc)
          .to_a.uniq { |h| [h.provider_security_id || h.security_id, h.currency] }
        previous.each do |holding|
          security = holding.provider_security || holding.security
          next unless security.ticker.match?(DegitideSimplefin::SYMBOL)
          next if open_symbols.include?(security.ticker)
          next if holding.qty.zero? && holding.amount.zero?
          occupied = account.holdings.where(date: snapshot_date, currency: holding.currency,
            security_id: [security.id, holding.security_id]).to_a
          next if occupied.any? { |row| row.account_provider_id != provider.id }

          # A dated zero-position row removes a closed position from Sure's
          # current_holdings without deleting its historical holdings or trades.
          holding.with_lock do
            next unless holding.account_provider_id == provider.id
            closed = holding.date == snapshot_date ? holding : holding.dup
            closed.assign_attributes(qty: 0, amount: 0, price: 0, date: snapshot_date,
              external_id: "degitide_closed_#{provider.id}_#{security.id}_#{snapshot_date}")
            Holding.transaction(requires_new: true) { closed.save! }
          end
        rescue ActiveRecord::RecordNotUnique
          next
        end
        DegitideSimplefin::ValuationsProcessor.new(simplefin_account).process
      end
    end
  end

  module CurrentHoldings
    def current_holdings
      current = super
      return current unless accountable_type == "Investment"
      snapshots = account_providers.where(provider_type: "SimplefinAccount").includes(:provider).filter_map do |link|
        payload = link.provider.raw_payload
        [link.id, payload] if DegitideSimplefin.complete?(payload, check_time: false, check_history: false)
      end.to_h
      return current if snapshots.empty?

      closed = current.includes(:security, :provider_security).filter_map do |holding|
        payload = snapshots[holding.account_provider_id]
        next unless payload && holding.currency == payload["currency"]
        symbol = (holding.provider_security || holding.security).ticker
        next unless symbol.match?(DegitideSimplefin::SYMBOL)
        holding.id unless payload["holdings"].any? { |row| row["symbol"] == symbol }
      end
      current.where.not(id: closed)
    end
  end
end

Rails.application.config.to_prepare do
  importer = SimplefinItem::Importer
  holdings = SimplefinAccount::Investments::HoldingsProcessor
  importer.prepend(DegitideSimplefin::Importer) unless importer < DegitideSimplefin::Importer
  holdings.prepend(DegitideSimplefin::HoldingsProcessor) unless holdings < DegitideSimplefin::HoldingsProcessor
  Account.prepend(DegitideSimplefin::CurrentHoldings) unless Account < DegitideSimplefin::CurrentHoldings
  normalizer = Balance::LinkedInvestmentSeriesNormalizer
  normalizer.prepend(DegitideSimplefin::SupportedHistory) unless normalizer < DegitideSimplefin::SupportedHistory
  normalizer.singleton_class.prepend(DegitideSimplefin::AggregateSupportedHistory) unless normalizer.singleton_class < DegitideSimplefin::AggregateSupportedHistory
end
