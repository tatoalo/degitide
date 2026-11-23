# Test-only control channel over docker exec stdin/stdout. Boot Rails once while
# retaining the real models, database, Sidekiq jobs and compatibility adapter.
require 'json'
require 'sidekiq/api'
STDOUT.sync = true
$stdout.sync = true
puts 'E2E_READY'
STDOUT.flush

STDIN.each_line do |line|
  begin
    command = JSON.parse(line).fetch('command')
    ActiveRecord::Base.uncached do
      case command
      when 'snapshot'
        load File.join(__dir__, 'inspect.rb')
      when 'idle'
        puts 'E2E_JSON:' + JSON.generate({
          busy: Sync.where(status: ['pending', 'syncing']).count,
          queued: Sidekiq::Queue.all.sum(&:size)
        })
      when 'schedule'
        SyncAllJob.perform_later
        puts 'E2E_JSON:{}'
      when 'guards'
        load File.join(__dir__, 'guards.rb')
        puts 'E2E_JSON:{}'
      else
        raise "Unknown test command: #{command}"
      end
    end
  rescue StandardError => error
    puts 'E2E_JSON:' + JSON.generate(error: "#{error.class}: #{error.message}")
  ensure
    STDOUT.flush
  end
end
