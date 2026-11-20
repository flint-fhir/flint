#!/usr/bin/env ruby
# frozen_string_literal: true

# Headless HL7 AU Core (v1.0.0 & v2.0.0) Inferno execution harness for Flint CI.
# Runs au_core_test_kit in isolation because its tls_test_kit ~> 0.2.0 dependency
# conflicts with tls_test_kit ~> 1.0 in deploy/inferno/headless/Gemfile.

require 'fileutils'
require 'json'
require 'time'

inferno_bind_port = ENV.fetch('INFERNO_PORT', '14570')
if ENV['INFERNO_HOST'].to_s.strip.empty?
  ENV['INFERNO_HOST'] = "http://127.0.0.1:#{inferno_bind_port}"
elsif !ENV['INFERNO_HOST'].include?('://')
  ENV['INFERNO_HOST'] = "http://#{ENV['INFERNO_HOST']}:#{inferno_bind_port}"
end
ENV['BASE_URL'] ||= ENV['INFERNO_HOST']
ENV['NO_DB'] = 'false'
ENV['ASYNC_JOBS'] = 'false'
ENV['INFERNO_DISABLE_TLS_TEST'] ||= 'true'
ENV['FHIR_RESOURCE_VALIDATOR_URL'] ||= 'http://127.0.0.1:14568'
ENV['FHIRPATH_URL'] ||= ENV['FHIR_RESOURCE_VALIDATOR_URL']

FileUtils.mkdir_p(File.join(__dir__, 'data'))
$LOAD_PATH.unshift(File.join(__dir__, 'lib')) unless $LOAD_PATH.include?(File.join(__dir__, 'lib'))

require 'inferno'
Inferno::Application.finalize!
require 'flint_inferno_au_core' if Inferno::Repositories::TestSuites.new.all.empty?

# Upstream bug workaround (hl7au/au-fhir-core-inferno#305):
# InfernoSuiteGenerator::ChainedSearchTest#run_chain_search_test hardcodes
# scratch[:patient_resources] even when the chained target resource is not
# Patient (specifically practitioner:Practitioner.identifier in PractitionerRole),
# issuing zero HTTP requests before failing. For non-Patient chain targets,
# resolve candidate target resources from scratch[:<target>_resources][:all]
# so Inferno actually issues and validates the live chained search against flintd.
if defined?(InfernoSuiteGenerator::ChainedSearchTest) &&
   !InfernoSuiteGenerator::ChainedSearchTest.method_defined?(:flint_orig_run_chain_search_test)
  InfernoSuiteGenerator::ChainedSearchTest.module_eval do
    alias_method :flint_orig_run_chain_search_test, :run_chain_search_test

    def run_chain_search_test
      search_param = search_param_names[0]
      chain_target = extract_target_resource_from_chained_search_parameter(search_param)
      return flint_orig_run_chain_search_test if chain_target == 'Patient'

      scratch_key = :"#{chain_target.gsub(/([a-z\d])([A-Z])/, '\1_\2').downcase}_resources"
      target_bucket = scratch[scratch_key] || scratch[scratch_key.to_s] || {}
      target_resources = Array(target_bucket[:all] || target_bucket['all'] || target_bucket.values.flatten).select do |r|
        r.respond_to?(:resourceType) && r.resourceType == chain_target && r.respond_to?(:id) && r.id
      end
      target_id_list = target_resources.map(&:id).uniq
      resources_by_id = target_resources.group_by(&:id)

      run_chain_search_test_clean(
        search_param,
        target_id_list,
        resources_by_id,
        attr_paths,
        target_identifier
      )
    end
  end
end

flint_base_url = ENV.fetch('FLINT_BASE_URL', 'http://127.0.0.1:18080').chomp('/')
au_tenant      = ENV.fetch('FLINT_AU_TENANT', 'aucore')
au_fhir_url    = ENV.fetch('FLINT_AU_FHIR_URL', "#{flint_base_url}/fhir/r4/#{au_tenant}").chomp('/')
patient_ids    = ENV.fetch('INFERNO_AU_PATIENT_IDS', 'pat-au-001,pat-au-002')
access_token   = ENV.fetch('INFERNO_ACCESS_TOKEN', '').strip
artifacts_dir  = ENV.fetch('INFERNO_ARTIFACTS_DIR', File.expand_path('../../../artifacts/inferno-au-core', __dir__))

FileUtils.mkdir_p(artifacts_dir)

def execute_au_core_suite(suite_id:, au_fhir_url:, patient_ids:, access_token:, artifacts_dir:)
  suites_repo   = Inferno::Repositories::TestSuites.new
  tests_repo    = Inferno::Repositories::Tests.new
  sessions_repo = Inferno::Repositories::TestSessions.new
  data_repo     = Inferno::Repositories::SessionData.new
  runs_repo     = Inferno::Repositories::TestRuns.new
  messages_repo = Inferno::Repositories::Messages.new

  suite = suites_repo.find(suite_id)
  raise "AU Core Inferno suite '#{suite_id}' is not registered" if suite.nil?

  test_session = sessions_repo.create(test_suite_id: suite.id)
  available = suite.available_inputs
  available_list = available.is_a?(Hash) ? available.values : Array(available)

  oauth_json = access_token.empty? ? '' : JSON.generate({ access_token: access_token })
  inputs = {
    url: au_fhir_url,
    smart_credentials: oauth_json,
    patient_ids: patient_ids,
    healthcare_service_ids: 'hs-au-001',
    location_ids: 'loc-au-001',
    organization_ids: 'org-au-001',
    practitioner_ids: 'prac-au-001,prac-au-002',
    practitioner_role_ids: 'pr-au-001,pr-au-002'
  }

  inputs.each do |k, v|
    matched = available_list.find { |ri| ri.name.to_s == k.to_s }
    next unless matched

    val_to_save = v
    if matched.type.to_s == 'oauth_credentials' && val_to_save.to_s.strip.empty?
      next
    end

    data_repo.save(
      test_session_id: test_session.id,
      name: matched.name,
      value: val_to_save,
      type: matched.type
    )
  end

  started_at = Time.now
  test_run = runs_repo.create(
    test_session_id: test_session.id,
    test_suite_id: suite.id,
    status: 'queued'
  )

  puts ">>> Running #{suite_id} against #{au_fhir_url} ..."
  Inferno::Jobs.perform(Inferno::Jobs::ExecuteTestRun, test_run.id, force_synchronous: true)
  elapsed_s = (Time.now - started_at).round(2)

  results = runs_repo.results_for_test_run(test_run.id)
  leaf_results = results.select { |r| !r.test_id.nil? && !r.test_id.to_s.empty? }

  counts = Hash.new(0)
  leaf_results.each { |r| counts[r.result.to_s] += 1 }

  resolve_messages = lambda do |res_obj|
    msgs = Array(res_obj.respond_to?(:messages) ? res_obj.messages : nil)
    if msgs.empty? && messages_repo.respond_to?(:messages_for_result)
      msgs = Array(messages_repo.messages_for_result(res_obj.id))
    end
    msgs
  end

  resolve_test_entity = lambda do |res_obj|
    (res_obj.respond_to?(:runnable) && res_obj.runnable) || tests_repo.find(res_obj.test_id)
  end

  failures = leaf_results.select { |r| %w[fail error wait cancel].include?(r.result.to_s) }
  failures.each do |f|
    ent = resolve_test_entity.call(f)
    title = ent&.title || f.test_id
    warn "  [#{f.result.upcase}] #{f.test_id} (#{title}): #{f.result_message}"
    resolve_messages.call(f).each do |m|
      warn "    (#{m.type}) #{m.message}"
    end
  end

  payload = {
    label: suite_id,
    suite_id: suite.id,
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
      ent = resolve_test_entity.call(r)
      {
        test_id: r.test_id,
        short_id: ent&.short_id,
        title: ent&.title,
        result: r.result,
        result_message: r.result_message,
        messages: resolve_messages.call(r).map { |m| { type: m.type, message: m.message } }
      }
    end
  }
  File.write(File.join(artifacts_dir, "#{suite_id}.json"), JSON.pretty_generate(payload))
  puts format(
    '<<< %<label>-20s total=%<tot>3d pass=%<pass>3d skip=%<skip>3d omit=%<omit>2d fail=%<fail>2d error=%<err>2d (%<sec>.2fs)',
    label: suite_id,
    tot: leaf_results.size,
    pass: counts['pass'],
    skip: counts['skip'],
    omit: counts['omit'],
    fail: counts['fail'],
    err: counts['error'],
    sec: elapsed_s
  )
  payload
end

runs = %w[au_core_v100 au_core_v200].map do |sid|
  execute_au_core_suite(
    suite_id: sid,
    au_fhir_url: au_fhir_url,
    patient_ids: patient_ids,
    access_token: access_token,
    artifacts_dir: artifacts_dir
  )
end

File.write(
  File.join(artifacts_dir, 'summary.json'),
  JSON.pretty_generate({
    generated_at: Time.now.utc.iso8601,
    au_fhir_url: au_fhir_url,
    suites: runs.map { |r| r.reject { |k, _| k == :tests } }
  })
)

puts "\n================ HL7 AU CORE INFERNO SCORECARD ================="
failed = []
runs.each do |r|
  c = r[:counts]
  ok = c[:fail].zero? && c[:error].zero? && c[:wait].zero? && c[:cancel].zero? && c[:pass].positive?
  failed << r[:label] unless ok
  puts format(
    '  [%<st>s] %<lbl>-20s pass=%<pass>3d  skip=%<skip>3d  omit=%<omit>2d  fail=%<fail>2d  error=%<err>2d',
    st: ok ? 'PASS' : 'FAIL',
    lbl: r[:label],
    pass: c[:pass],
    skip: c[:skip],
    omit: c[:omit],
    fail: c[:fail],
    err: c[:error]
  )
end
puts '================================================================'

unless failed.empty?
  warn "AU Core Inferno conformance failures detected in: #{failed.join(', ')}"
  exit 1
end
