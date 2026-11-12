#!/usr/bin/env ruby
# frozen_string_literal: true

# Headless HL7 Inferno execution harness for Flint CI.
#
# Runs three official HL7 Inferno test kits synchronously in-process (SQLite, no Redis/Sidekiq):
#   1. SMART App Launch STU2 (`smart_stu2` -> Backend Services & Discovery)
#   2. FHIR Bulk Data Access v2.0.0 (`bulk_data_v200` -> SMART Backend Services + Group/Patient/System $export)
#   3. US Core Server v6.1.0 (`us_core_v610` -> CapabilityStatement + US Core v6.1.0 FHIR API clinical profiles)

require 'fileutils'
require 'json'
require 'net/http'
require 'uri'

ENV['NO_DB'] = 'false'
ENV['ASYNC_JOBS'] = 'false'
ENV['INFERNO_DISABLE_TLS_TEST'] ||= 'true'
ENV['FHIR_RESOURCE_VALIDATOR_URL'] ||= 'http://127.0.0.1:14568'

FileUtils.mkdir_p(File.join(__dir__, 'data'))
$LOAD_PATH.unshift(File.join(__dir__, 'lib')) unless $LOAD_PATH.include?(File.join(__dir__, 'lib'))

require 'inferno'
Inferno::Application.finalize!
require 'flint_inferno_headless' if Inferno::Repositories::TestSuites.new.all.empty?

flint_base_url = ENV.fetch('FLINT_BASE_URL', 'http://127.0.0.1:18080').chomp('/')
flint_tenant   = ENV.fetch('FLINT_TENANT', 'demo')
flint_fhir_url = ENV.fetch('FLINT_FHIR_URL', "#{flint_base_url}/fhir/r4/#{flint_tenant}").chomp('/')
patient_ids    = ENV.fetch('INFERNO_PATIENT_IDS', 'pat-alice-001,pat-robert-002,pat-elena-003')
group_id       = ENV.fetch('INFERNO_GROUP_ID', 'all')
client_id      = ENV.fetch('INFERNO_CLIENT_ID', 'inferno-bulk-client')
access_token   = ENV.fetch('INFERNO_ACCESS_TOKEN', '').strip
artifacts_dir  = ENV.fetch('INFERNO_ARTIFACTS_DIR', File.expand_path('../../../artifacts/inferno', __dir__))

FileUtils.mkdir_p(artifacts_dir)

def collect_inferno_public_jwks
  sources = []
  sources << JSON.parse(Inferno::JWKS.jwks_json) if defined?(Inferno::JWKS)
  sources << JSON.parse(SMARTAppLaunch::JWKS.jwks_json) if defined?(SMARTAppLaunch::JWKS)
  if defined?(BulkDataTestKit::BulkDataV200::BulkDataTestSuite)
    sources << JSON.parse(BulkDataTestKit::BulkDataV200::BulkDataTestSuite.jwks_json)
  end

  seen_kids = {}
  merged = []
  sources.each do |doc|
    Array(doc['keys']).each do |key|
      kid = key['kid'].to_s
      dedup_key = kid.empty? ? JSON.generate(key) : kid
      next if seen_kids[dedup_key]

      seen_kids[dedup_key] = true
      merged << key
    end
  end
  merged
end

def register_jwks_with_flint!(flint_base_url, client_id)
  keys = collect_inferno_public_jwks
  uri = URI.parse("#{flint_base_url}/oauth2/register")
  req = Net::HTTP::Post.new(uri)
  req['Content-Type'] = 'application/json'
  req.body = JSON.generate({
    client_id: client_id,
    client_name: 'HL7 Inferno Headless Conformance Runner',
    token_endpoint_auth_method: 'private_key_jwt',
    grant_types: ['client_credentials'],
    scope: 'system/*.rs system/*.read system/*.cruds',
    jwks: { keys: keys }
  })

  resp = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == 'https', read_timeout: 15) do |http|
    http.request(req)
  end
  unless resp.code.to_i.between?(200, 299)
    raise "Failed to register Inferno JWKS at #{uri} (HTTP #{resp.code}): #{resp.body}"
  end

  puts "Registered #{keys.size} Inferno public signing keys for client_id=#{client_id}"
end

def execute_inferno_target(label:, suite_id:, group_suffix: nil, suite_options: {}, inputs: {}, artifacts_dir:)
  suites_repo   = Inferno::Repositories::TestSuites.new
  groups_repo   = Inferno::Repositories::TestGroups.new
  sessions_repo = Inferno::Repositories::TestSessions.new
  data_repo     = Inferno::Repositories::SessionData.new
  runs_repo     = Inferno::Repositories::TestRuns.new

  suite = suites_repo.find(suite_id)
  raise "Inferno suite '#{suite_id}' is not registered" if suite.nil?

  runnable = if group_suffix.nil?
               suite
             else
               groups_repo.all.find do |g|
                 g.suite&.id == suite.id && (
                   g.id.to_s == group_suffix.to_s ||
                   g.id.to_s.end_with?("-#{group_suffix}") ||
                   g.short_id.to_s == group_suffix.to_s
                 )
               end
             end
  raise "Group '#{group_suffix}' not found in suite '#{suite_id}'" if runnable.nil?

  opt_objs = suite_options.map do |k, v|
    Inferno::DSL::SuiteOption.new(id: k.to_sym, value: v.to_s)
  end
  test_session = sessions_repo.create(
    test_suite_id: suite.id,
    suite_options: opt_objs
  )

  available = runnable.available_inputs
  persisted_inputs = []
  inputs.each do |k, v|
    matched = available.find { |_, ri| ri.name.to_s == k.to_s }&.last
    if matched
      data_repo.save(
        test_session_id: test_session.id,
        name: matched.name,
        value: v,
        type: matched.type
      )
      persisted_inputs << { name: matched.name, value: v }
    else
      persisted_inputs << { name: k.to_s, value: v }
    end
  end

  runnable_key = runnable < Inferno::TestSuite ? :test_suite_id : :test_group_id
  test_run = runs_repo.create(
    test_session_id: test_session.id,
    runnable_key => runnable.id,
    inputs: persisted_inputs,
    status: 'queued'
  )

  started_at = Time.now
  puts "\n>>> Running #{label} (#{runnable.id}) ..."
  Inferno::Jobs.perform(Inferno::Jobs::ExecuteTestRun, test_run.id, force_synchronous: true)
  elapsed_s = (Time.now - started_at).round(2)

  results = runs_repo.results_for_test_run(test_run.id)
  leaf_results = results.select { |r| !r.test_id.nil? && !r.test_id.to_s.empty? }

  counts = Hash.new(0)
  leaf_results.each { |r| counts[r.result.to_s] += 1 }

  failures = leaf_results.select { |r| %w[fail error wait cancel].include?(r.result.to_s) }
  failures.each do |f|
    title = f.runnable&.title || f.test_id
    warn "  [#{f.result.upcase}] #{f.test_id} (#{title}): #{f.result_message}"
    Array(f.messages).each do |m|
      warn "    (#{m.type}) #{m.message}"
    end
  end

  artifact_payload = {
    label: label,
    suite_id: suite.id,
    runnable_id: runnable.id,
    elapsed_seconds: elapsed_s,
    counts: {
      total_leaf_tests: leaf_results.size,
      pass: counts['pass'],
      skip: counts['skip'],
      omit: counts['omit'],
      fail: counts['fail'],
      error: counts['error'],
      wait: counts['wait'],
      cancel: counts['cancel']
    },
    tests: leaf_results.map do |r|
      {
        test_id: r.test_id,
        short_id: r.runnable&.short_id,
        title: r.runnable&.title,
        result: r.result,
        result_message: r.result_message,
        messages: Array(r.messages).map { |m| { type: m.type, message: m.message } }
      }
    end
  }

  File.write(File.join(artifacts_dir, "#{label}.json"), JSON.pretty_generate(artifact_payload))
  puts format(
    '<<< %<label>-20s total=%<tot>3d pass=%<pass>3d skip=%<skip>3d omit=%<omit>2d fail=%<fail>2d error=%<err>2d (%<sec>.2fs)',
    label: label,
    tot: leaf_results.size,
    pass: counts['pass'],
    skip: counts['skip'],
    omit: counts['omit'],
    fail: counts['fail'],
    err: counts['error'],
    sec: elapsed_s
  )

  artifact_payload
end

register_jwks_with_flint!(flint_base_url, client_id)

backend_auth_json = JSON.generate({
  auth_type: 'backend_services',
  use_discovery: 'true',
  token_url: "#{flint_base_url}/oauth2/token",
  client_id: client_id,
  encryption_algorithm: 'ES384',
  requested_scopes: 'system/*.rs'
})

bulk_auth_json = JSON.generate({
  auth_type: 'backend_services',
  use_discovery: 'true',
  token_url: "#{flint_base_url}/oauth2/token",
  client_id: client_id,
  encryption_algorithm: 'ES384',
  requested_scopes: 'system/*.read',
  access_token: access_token
})

us_core_auth_json = JSON.generate({
  auth_type: 'public',
  access_token: access_token
})

runs = []

runs << execute_inferno_target(
  label: 'smart_stu2',
  suite_id: 'smart_stu2',
  group_suffix: 'smart_backend_services',
  inputs: {
    url: flint_fhir_url,
    backend_services_smart_auth_info: backend_auth_json
  },
  artifacts_dir: artifacts_dir
)

runs << execute_inferno_target(
  label: 'bulk_data_v200',
  suite_id: 'bulk_data_v200',
  group_suffix: nil,
  inputs: {
    bulk_server_url: flint_fhir_url,
    group_id: group_id,
    bulk_timeout: '60',
    smart_auth_info: bulk_auth_json
  },
  artifacts_dir: artifacts_dir
)

runs << execute_inferno_target(
  label: 'us_core_v610',
  suite_id: 'us_core_v610',
  group_suffix: 'us_core_v610_fhir_api',
  suite_options: {
    smart_app_launch_version: 'smart_app_launch_2'
  },
  inputs: {
    url: flint_fhir_url,
    patient_ids: patient_ids,
    smart_auth_info: us_core_auth_json
  },
  artifacts_dir: artifacts_dir
)

summary = {
  generated_at: Time.now.utc.iso8601,
  flint_fhir_url: flint_fhir_url,
  suites: runs.map { |r| r.reject { |k, _| k == :tests } }
}
File.write(File.join(artifacts_dir, 'summary.json'), JSON.pretty_generate(summary))

puts "\n================ HL7 INFERNO HEADLESS SCORECARD ================"
failed_suites = []
runs.each do |r|
  c = r[:counts]
  status = if c[:fail].zero? && c[:error].zero? && c[:wait].zero? && c[:cancel].zero? && c[:pass].positive?
             'PASS'
           else
             failed_suites << r[:label]
             'FAIL'
           end
  puts format(
    '  [%<st>s] %<lbl>-18s pass=%<pass>3d  skip=%<skip>3d  omit=%<omit>2d  fail=%<fail>2d  error=%<err>2d',
    st: status,
    lbl: r[:label],
    pass: c[:pass],
    skip: c[:skip],
    omit: c[:omit],
    fail: c[:fail],
    err: c[:error]
  )
end
puts '================================================================'

unless failed_suites.empty?
  warn "Inferno conformance failures detected in: #{failed_suites.join(', ')}"
  exit 1
end
