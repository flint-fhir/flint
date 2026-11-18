#!/usr/bin/env ruby
# frozen_string_literal: true

# Headless HL7 Inferno execution harness for Flint CI.
#
# Runs official HL7 & ONC Inferno test suites synchronously in-process (SQLite, no Redis/Sidekiq):
#   1. SMART App Launch STU2 (`smart_stu2` -> Full Suite: Discovery, Standalone Launch PKCE/OIDC/Refresh,
#      EHR Launch Symmetric/Authorize-POST/OIDC/Refresh, Backend Services ES384, RFC 7662 Token Introspection)
#   2. SMART App Launch STU2 Asymmetric (`smart_stu2_asymmetric` -> Confidential Asymmetric `private_key_jwt`
#      Standalone Launch, OIDC, and Refresh Token Rotation)
#   3. FHIR Bulk Data Access v2.0.0 (`bulk_data_v200` -> SMART Backend Services + Group/Patient/System $export)
#   4. US Core Server v6.1.0 (`us_core_v610` -> CapabilityStatement + all 26 US Core v6.1.0 FHIR API clinical profiles)
#   5. US Core Server v7.0.0 (`us_core_v700` -> CapabilityStatement + all 26 US Core v7.0.0 / USCDI v4 FHIR API clinical profiles)
#   6. US Core Server v8.0.0 (`us_core_v800` -> CapabilityStatement + all US Core v8.0.0 / USCDI v5 + ADI clinical profiles)
#   7. HL7 International Patient Access v1.0.0 (`ipa_v100` -> CapabilityStatement + IPA FHIR API clinical profiles)
#   8. HL7 Subscriptions R5 Backport IG v1.1.0 (`subscriptions_r5_backport_r4_server` -> Workflow, CapabilityStatement,
#      Empty/ID-Only/Full-Resource Notifications, Handshake/Heartbeat, $status, 7 Rejection cases, PUT/PATCH/DELETE)
#   9. ONC § 170.315(g)(10) / HTI-1 Standardized API (`g10_certification` -> Standalone, Limited, EHR Practitioner,
#      Single Patient US Core v7, Multi-Patient Bulk Data STU2, and Group06 Authorization & Fine-Grained Scopes)

require 'base64'
require 'digest'
require 'fileutils'
require 'json'
require 'net/http'
require 'rack/mock'
require 'securerandom'
require 'uri'

inferno_bind_port = ENV.fetch('INFERNO_PORT', '14569')
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
require 'pas_payloads'
require 'track8_payloads'

require 'inferno'
Inferno::Application.finalize!
require 'flint_inferno_headless' if Inferno::Repositories::TestSuites.new.all.empty?
require 'inferno/apps/web/application'

if defined?(SubscriptionsTestKit::SubscriptionRestHookEndpoint)
  SubscriptionsTestKit::SubscriptionRestHookEndpoint.prepend(
    Module.new do
      def make_response
        response.format = :json
        super
      end
    end
  )
end

if defined?(Inferno::DSL::FhirpathEvaluation::Evaluator)
  Inferno::DSL::FhirpathEvaluation::Evaluator.prepend(
    Module.new do
      def call_fhirpath_service(fhir_resource, fhirpath_expression)
        super(fhir_resource, URI.encode_www_form_component(fhirpath_expression.to_s))
      end
    end
  )
end

$active_crd_job_threads = []
$active_crd_job_threads_mutex = Mutex.new

if defined?(DaVinciCRDTestKit::Jobs::InvokeHook)
  Inferno::Jobs.singleton_class.prepend(
    Module.new do
      def perform(job_class, *args, **kwargs)
        if job_class.is_a?(Class) && job_class <= DaVinciCRDTestKit::Jobs::InvokeHook
          thr = Thread.new { super(job_class, *args, **kwargs) }
          $active_crd_job_threads_mutex.synchronize { $active_crd_job_threads << thr }
          thr
        else
          super
        end
      end
    end
  )
end

if defined?(CancerRegistryReportingTestKit::HdeaGenerator::GroupMetadata)
  CancerRegistryReportingTestKit::HdeaGenerator::GroupMetadata.prepend(
    Module.new do
      def initialize(metadata)
        super
        must_supports[:extensions]&.reject! { |e| e[:url].nil? } if must_supports.is_a?(Hash)
      end
    end
  )
end

if defined?(CancerRegistryReportingTestKit::HDEAV100::PractitionerRoleMustSupportTest)
  CancerRegistryReportingTestKit::HDEAV100::PractitionerRoleMustSupportTest.singleton_class.prepend(
    Module.new do
      def metadata
        @metadata ||= begin
          src = instance_method(:resource_type).source_location&.first
          yml_path = File.join(File.dirname(src.to_s), 'practitioner_role_metadata.yml')
          CancerRegistryReportingTestKit::HdeaGenerator::GroupMetadata.new(YAML.load_file(yml_path, aliases: true))
        end
      end
    end
  )
end

if defined?(CancerRegistryReportingTestKit::CCRRV100::HDEASuite) &&
   defined?(USCoreTestKit::ProvenanceValidator) &&
   !CancerRegistryReportingTestKit::CCRRV100::HDEASuite.const_defined?(:ProvenanceValidator)
  CancerRegistryReportingTestKit::CCRRV100::HDEASuite.const_set(:ProvenanceValidator, USCoreTestKit::ProvenanceValidator)
end

if defined?(SMART_UDAP_HarmonizationTestKit::SMART_UDAP_TokenRefreshTest)
  SMART_UDAP_HarmonizationTestKit::SMART_UDAP_TokenRefreshTest.class_eval do
    def udap_received_scopes
      received_scopes
    end
  end
end

if defined?(SMART_UDAP_HarmonizationTestKit::SMART_UDAP_TokenResponseScopeTest)
  SMART_UDAP_HarmonizationTestKit::SMART_UDAP_TokenResponseScopeTest.output :udap_refresh_token
  SMART_UDAP_HarmonizationTestKit::SMART_UDAP_TokenResponseScopeTest.prepend(
    Module.new do
      def check_for_missing_scopes(req_scopes, parsed_body)
        output(udap_refresh_token: parsed_body['refresh_token']) if parsed_body.is_a?(Hash) && parsed_body['refresh_token']
        super
      end
    end
  )
end

flint_base_url    = ENV.fetch('FLINT_BASE_URL', 'http://127.0.0.1:18080').chomp('/')
flint_tenant      = ENV.fetch('FLINT_TENANT', 'demo')
flint_fhir_url    = ENV.fetch('FLINT_FHIR_URL', "#{flint_base_url}/fhir/r4/#{flint_tenant}").chomp('/')
patient_ids       = ENV.fetch('INFERNO_PATIENT_IDS', 'pat-alice-001,pat-robert-002,pat-elena-003')
launch_patient_id = patient_ids.split(',').first.to_s.strip
launch_patient_id = 'pat-alice-001' if launch_patient_id.empty?
launch_enc_id     = ENV.fetch('INFERNO_ENCOUNTER_ID', 'enc-alice-001')
group_id          = ENV.fetch('INFERNO_GROUP_ID', 'all')
client_id         = ENV.fetch('INFERNO_CLIENT_ID', 'inferno-bulk-client')
asym_client_id    = ENV.fetch('INFERNO_ASYM_CLIENT_ID', 'inferno-smart-asymmetric')
ehr_client_id     = ENV.fetch('INFERNO_EHR_CLIENT_ID', 'inferno-ehr-confidential')
ehr_client_secret = ENV.fetch('INFERNO_EHR_CLIENT_SECRET', 'inferno-ehr-secret-stu2-key')
g10_client_id     = ENV.fetch('INFERNO_G10_CLIENT_ID', 'inferno-g10-confidential')
g10_client_secret = ENV.fetch('INFERNO_G10_CLIENT_SECRET', 'inferno-g10-secret-stu2-key')
g10_bulk_client   = ENV.fetch('INFERNO_G10_BULK_CLIENT_ID', 'inferno-g10-bulk-client')
access_token      = ENV.fetch('INFERNO_ACCESS_TOKEN', '').strip
artifacts_dir     = ENV.fetch('INFERNO_ARTIFACTS_DIR', File.expand_path('../../../artifacts/inferno', __dir__))

FileUtils.mkdir_p(artifacts_dir)

def build_inferno_base_rack_app
  core_root = Gem.loaded_specs['inferno_core']&.full_gem_path
  if core_root
    Dir.glob(File.join(core_root, 'lib', 'inferno', 'utils', 'middleware', '*.rb')).sort.each do |mw_file|
      require mw_file
    end

    %w[config.ru lib/inferno/config.ru lib/inferno/apps/web/config.ru].each do |rel|
      ru_path = File.join(core_root, rel)
      next unless File.file?(ru_path)

      begin
        parsed = Rack::Builder.parse_file(ru_path)
        puts "Loaded Inferno Rack stack from #{ru_path}"
        return parsed.is_a?(Array) ? parsed.first : parsed
      rescue StandardError => e
        warn "Fallback from #{ru_path}: #{e.message}"
      end
    end
  end

  app = Inferno::Web.app
  mounted = []
  if defined?(Inferno::Utils::Middleware)
    Inferno::Utils::Middleware.constants.sort.each do |const_name|
      mw = Inferno::Utils::Middleware.const_get(const_name)
      next unless mw.is_a?(Class) && mw.instance_methods.include?(:call)

      arity = mw.instance_method(:initialize).arity
      next unless [1, -1, -2].include?(arity)

      app = mw.new(app)
      mounted << const_name.to_s
    end
  end
  puts "Mounted Inferno Rack middlewares: #{mounted.empty? ? '(none)' : mounted.join(', ')}"
  app
end

INFERNO_BASE_APP = build_inferno_base_rack_app
SUITE_ENDPOINT_LOC = Inferno::DSL::SuiteEndpoint.instance_method(:add_persistence_callback).source_location
SUITE_ENDPOINT_SRC = SUITE_ENDPOINT_LOC && File.file?(SUITE_ENDPOINT_LOC.first) ? File.read(SUITE_ENDPOINT_LOC.first) : ''
SUITE_ENDPOINT_ENV_KEYS = SUITE_ENDPOINT_SRC.scan(/response_headers\s*=\s*(?:request\.)?env\[['"]([^'"]+)['"]\]/).flatten.uniq

base_uri = URI.parse(Inferno::Application['base_url'].to_s)
base_prefix = base_uri.path.to_s.chomp('/')
INFERNO_RACK_MUTEX = Mutex.new
INFERNO_RACK_APP = lambda do |env|
  INFERNO_RACK_MUTEX.synchronize do
    after_reply = []
    response_finished = []
    inner_env = env.merge(
      'rack.after_reply' => after_reply,
      'rack.response_finished' => response_finished
    )
    if !base_prefix.empty? && inner_env['PATH_INFO'].to_s.start_with?(base_prefix)
      stripped = inner_env['PATH_INFO'].to_s.sub(/\A#{Regexp.escape(base_prefix)}/, '')
      stripped = '/' if stripped.empty?
      inner_env = inner_env.merge('SCRIPT_NAME' => base_prefix, 'PATH_INFO' => stripped)
    end
    status, headers, body = INFERNO_BASE_APP.call(inner_env)
    safe_headers = (headers || {}).to_h
    SUITE_ENDPOINT_ENV_KEYS.each { |k| inner_env[k] ||= safe_headers }
    response_finished.each do |cb|
      if cb.arity.zero?
        cb.call
      else
        cb.call(inner_env, status, safe_headers, nil)
      end
    end
    after_reply.each do |cb|
      begin
        if cb.arity.zero?
          cb.call
        else
          cb.call(inner_env, status, safe_headers, nil)
        end
      rescue StandardError => e
        if SUITE_ENDPOINT_LOC && File.file?(SUITE_ENDPOINT_LOC.first)
          lines = File.readlines(SUITE_ENDPOINT_LOC.first)
          from_ln = [SUITE_ENDPOINT_LOC[1] - 5, 0].max
          to_ln = [from_ln + 45, lines.size - 1].min
          warn "--- suite_endpoint.rb (#{from_ln + 1}..#{to_ln + 1}) ---"
          (from_ln..to_ln).each { |idx| warn format('%4d: %s', idx + 1, lines[idx].to_s.chomp) }
        end
        raise e
      end
    end
    [status, safe_headers, body]
  end
end

INFERNO_RACK_MOCK = Rack::MockRequest.new(INFERNO_RACK_APP)

def start_inferno_http_listener!(rack_app, base_uri)
  require 'puma'

  listen_host = base_uri.host.to_s
  listen_host = '127.0.0.1' if listen_host.empty? || listen_host == 'localhost'
  listen_port = base_uri.port || 14569

  puma_srv = Puma::Server.new(rack_app)
  puma_srv.add_tcp_listener(listen_host, listen_port)
  puma_srv.run

  puts "Started in-process Inferno HTTP webhook/JWKS listener at http://#{listen_host}:#{listen_port} (base_url=#{Inferno::Application['base_url']})"
  puma_srv
end

inferno_http_server = start_inferno_http_listener!(INFERNO_RACK_APP, base_uri)
at_exit do
  inferno_http_server&.stop(true)
rescue StandardError
  nil
end

def collect_inferno_public_jwks
  sources = []
  sources << JSON.parse(Inferno::JWKS.jwks_json) if defined?(Inferno::JWKS)
  sources << JSON.parse(SMARTAppLaunch::JWKS.jwks_json) if defined?(SMARTAppLaunch::JWKS)
  if defined?(BulkDataTestKit::BulkDataV200::BulkDataTestSuite)
    sources << JSON.parse(BulkDataTestKit::BulkDataV200::BulkDataTestSuite.jwks_json)
  end
  if defined?(ONCCertificationG10TestKit::BulkDataJWKSHelper)
    sources << JSON.parse(ONCCertificationG10TestKit::BulkDataJWKSHelper.public_jwks_json)
  end
  if defined?(DaVinciCRDTestKit::JWKS) && DaVinciCRDTestKit::JWKS.respond_to?(:jwks_json)
    sources << JSON.parse(DaVinciCRDTestKit::JWKS.jwks_json)
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
    client_name: 'HL7 Inferno Headless Asymmetric Client',
    token_endpoint_auth_method: 'private_key_jwt',
    grant_types: %w[client_credentials authorization_code refresh_token],
    scope: 'launch launch/patient openid fhirUser offline_access patient/*.rs user/*.rs system/*.rs system/*.read system/*.cruds',
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

def register_symmetric_client_with_flint!(flint_base_url, client_id, client_secret)
  uri = URI.parse("#{flint_base_url}/oauth2/register")
  req = Net::HTTP::Post.new(uri)
  req['Content-Type'] = 'application/json'
  req.body = JSON.generate({
    client_id: client_id,
    client_secret: client_secret,
    client_name: 'HL7 Inferno Headless Confidential Symmetric Client',
    token_endpoint_auth_method: 'client_secret_basic',
    grant_types: %w[authorization_code refresh_token],
    scope: 'launch launch/patient launch/encounter openid fhirUser offline_access user/*.cruds user/*.rs patient/*.rs patient/*.read'
  })

  resp = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == 'https', read_timeout: 15) do |http|
    http.request(req)
  end
  unless resp.code.to_i.between?(200, 299)
    raise "Failed to register symmetric client at #{uri} (HTTP #{resp.code}): #{resp.body}"
  end

  puts "Registered confidential symmetric credentials for client_id=#{client_id}"
end

def mint_and_revoke_standalone_token_pair!(
  flint_base_url:,
  flint_fhir_url:,
  client_id:,
  client_secret:,
  patient_id:
)
  verifier = SecureRandom.urlsafe_base64(32)
  challenge = Base64.urlsafe_encode64(Digest::SHA256.digest(verifier), padding: false)
  redirect_uri = "#{Inferno::Application['base_url']}/custom/smart/redirect"

  auth_uri = URI.parse("#{flint_base_url}/oauth2/authorize")
  auth_uri.query = URI.encode_www_form(
    response_type: 'code',
    client_id: client_id,
    redirect_uri: redirect_uri,
    scope: 'launch/patient openid fhirUser offline_access patient/Patient.rs',
    state: SecureRandom.hex(8),
    aud: flint_fhir_url,
    patient: patient_id,
    code_challenge: challenge,
    code_challenge_method: 'S256'
  )
  auth_resp = Net::HTTP.get_response(auth_uri)
  unless [302, 303].include?(auth_resp.code.to_i)
    raise "Failed to authorize standalone token pair for revocation test (HTTP #{auth_resp.code}): #{auth_resp.body}"
  end

  cb_uri = URI.parse(auth_resp['location'].to_s)
  code = URI.decode_www_form(cb_uri.query.to_s).to_h['code'].to_s
  raise 'Missing authorization code for revocation test' if code.empty?

  tok_uri = URI.parse("#{flint_base_url}/oauth2/token")
  tok_req = Net::HTTP::Post.new(tok_uri)
  tok_req.basic_auth(client_id, client_secret)
  tok_req.set_form_data(
    'grant_type' => 'authorization_code',
    'code' => code,
    'redirect_uri' => redirect_uri,
    'code_verifier' => verifier
  )
  tok_resp = Net::HTTP.start(tok_uri.hostname, tok_uri.port, use_ssl: tok_uri.scheme == 'https', read_timeout: 15) do |http|
    http.request(tok_req)
  end
  unless tok_resp.code.to_i == 200
    raise "Failed to exchange token pair for revocation test (HTTP #{tok_resp.code}): #{tok_resp.body}"
  end

  tok_doc = JSON.parse(tok_resp.body)
  rev_uri = URI.parse("#{flint_base_url}/oauth2/revoke")
  rev_req = Net::HTTP::Post.new(rev_uri)
  rev_req.basic_auth(client_id, client_secret)
  rev_req.set_form_data('token' => tok_doc.fetch('access_token'))
  rev_resp = Net::HTTP.start(rev_uri.hostname, rev_uri.port, use_ssl: rev_uri.scheme == 'https', read_timeout: 15) do |http|
    http.request(rev_req)
  end
  unless rev_resp.code.to_i == 200
    raise "Failed to revoke standalone access_token at #{rev_uri} (HTTP #{rev_resp.code}): #{rev_resp.body}"
  end

  {
    auth_type: 'symmetric',
    client_id: client_id,
    client_secret: client_secret,
    access_token: tok_doc.fetch('access_token'),
    refresh_token: tok_doc.fetch('refresh_token'),
    token_url: "#{flint_base_url}/oauth2/token"
  }
end

def mint_ehr_launch_token!(flint_base_url:, flint_tenant:, patient_id:, encounter_id:)
  uri = URI.parse("#{flint_base_url}/oauth2/launch")
  req = Net::HTTP::Post.new(uri)
  req['Content-Type'] = 'application/json'
  req.body = JSON.generate({
    patient: patient_id,
    encounter: encounter_id,
    tenant: flint_tenant
  })

  resp = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == 'https', read_timeout: 15) do |http|
    http.request(req)
  end
  unless resp.code.to_i.between?(200, 299)
    raise "Failed to mint EHR launch token at #{uri} (HTTP #{resp.code}): #{resp.body}"
  end

  doc = JSON.parse(resp.body)
  token = doc['launch'].to_s.strip
  raise "Missing launch token in #{uri} response: #{resp.body}" if token.empty?

  token
end

def trigger_subscription_encounter_event!(flint_fhir_url:, patient_id:, bearer_token:)
  uri = URI.parse("#{flint_fhir_url}/Encounter")
  req = Net::HTTP::Post.new(uri)
  req['Content-Type'] = 'application/fhir+json'
  req['Authorization'] = "Bearer #{bearer_token}" unless bearer_token.to_s.empty?
  req.body = JSON.generate({
    resourceType: 'Encounter',
    status: 'in-progress',
    class: {
      system: 'http://terminology.hl7.org/CodeSystem/v3-ActCode',
      code: 'IMP',
      display: 'inpatient encounter'
    },
    type: [
      {
        coding: [
          {
            system: 'http://snomed.info/sct',
            code: '32485007',
            display: 'Hospital admission'
          }
        ]
      }
    ],
    subject: {
      reference: "Patient/#{patient_id}"
    },
    period: {
      start: Time.now.utc.iso8601
    }
  })
  resp = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == 'https', read_timeout: 15) do |http|
    http.request(req)
  end
  unless resp.code.to_i.between?(200, 299)
    raise "Failed to create Encounter event for Subscription delivery (HTTP #{resp.code}): #{resp.body}"
  end
end

def drive_waiting_subscription_flow!(
  test_session_id:,
  waiting_result:,
  confirmation_url:,
  flint_fhir_url:,
  patient_id:,
  bearer_token:,
  expect_heartbeat: false
)
  requests_repo = Inferno::Repositories::Requests.new
  created_reqs = requests_repo.tagged_requests(test_session_id, ['subscription_creation'])
  latest_create = Array(created_reqs).select { |r| r.status.to_i == 201 }.last
  sub_id = latest_create ? JSON.parse(latest_create.response_body.to_s)['id'].to_s : ''
  raise 'Unable to determine created Subscription ID during NotificationDeliveryTest wait' if sub_id.empty?

  puts "    [Subscriptions R5 Backport] Waiting for handshake#{expect_heartbeat ? ' + heartbeat' : ''} on Subscription/#{sub_id} ..."
  deadline = Time.now + 8.0
  until Time.now > deadline
    hs = requests_repo.tagged_requests(test_session_id, ['handshake', sub_id])
    hb = expect_heartbeat ? requests_repo.tagged_requests(test_session_id, ['heartbeat', sub_id]) : [true]
    break if !Array(hs).empty? && !Array(hb).empty?

    sleep 0.15
  end

  unless waiting_result.test_id.to_s.include?('pas_server')
    puts "    [Subscriptions R5 Backport] Triggering Encounter admission event for Subscription/#{sub_id} ..."
    trigger_subscription_encounter_event!(
      flint_fhir_url: flint_fhir_url,
      patient_id: patient_id,
      bearer_token: bearer_token
    )

    ev_deadline = Time.now + 8.0
    until Time.now > ev_deadline
      ev = requests_repo.tagged_requests(test_session_id, ['event-notification', sub_id])
      break unless Array(ev).empty?

      sleep 0.15
    end
  end

  puts "    [Subscriptions R5 Backport] Confirming notification delivery for #{waiting_result.test_id}"
  rack_resp = INFERNO_RACK_MOCK.get(confirmation_url)
  unless [200, 302, 303].include?(rack_resp.status)
    raise "Inferno subscription resume_pass route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
  end
end

def drive_waiting_smart_flow!(
  test_session_id:,
  test_run_id:,
  runs_repo:,
  results_repo:,
  flint_base_url:,
  flint_fhir_url:,
  flint_tenant:,
  patient_id:,
  encounter_id:,
  bearer_token: '',
  granted_scope_override: nil,
  expect_heartbeat: false
)
  max_waits = 24
  waits_handled = 0

  while runs_repo.status_for_test_run(test_run_id).to_s == 'waiting'
    waits_handled += 1
    if waits_handled > max_waits
      raise "Exceeded #{max_waits} interactive wait resumptions for test_run=#{test_run_id}"
    end

    waiting_result = results_repo.find_waiting_result(test_run_id: test_run_id)
    raise "Test run #{test_run_id} is waiting, but no waiting leaf result was found" if waiting_result.nil?

    outputs = Array(waiting_result.outputs).each_with_object({}) do |entry, acc|
      acc[entry['name'].to_s] = entry['value'].to_s
    end

    launch_uri       = outputs['launch_uri']
    auth_url         = outputs['authorization_url'].to_s.empty? ? outputs['udap_authorization_redirect_url'] : outputs['authorization_url']
    confirmation_url = outputs['confirmation_url']
    continuation_url = outputs['continuation_url']

    if !confirmation_url.nil? && !confirmation_url.empty?
      drive_waiting_subscription_flow!(
        test_session_id: test_session_id,
        waiting_result: waiting_result,
        confirmation_url: confirmation_url,
        flint_fhir_url: flint_fhir_url,
        patient_id: patient_id,
        bearer_token: bearer_token,
        expect_heartbeat: expect_heartbeat
      )
    elsif !launch_uri.nil? && !launch_uri.empty?
      target_iss = outputs['target_iss'].to_s.strip
      target_iss = flint_fhir_url if target_iss.empty?
      launch_token = mint_ehr_launch_token!(
        flint_base_url: flint_base_url,
        flint_tenant: flint_tenant,
        patient_id: patient_id,
        encounter_id: encounter_id
      )

      parsed_launch = URI.parse(launch_uri)
      existing_q = URI.decode_www_form(parsed_launch.query || '')
      existing_q << ['iss', target_iss]
      existing_q << ['launch', launch_token]
      parsed_launch.query = URI.encode_www_form(existing_q)

      puts "    [SMART EHR Launch] Resuming #{waiting_result.test_id} via #{parsed_launch.path}?iss=#{target_iss}&launch=#{launch_token}"
      rack_resp = INFERNO_RACK_MOCK.get(parsed_launch.to_s)
      unless [200, 302, 303].include?(rack_resp.status)
        raise "Inferno launch resume route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
      end
    elsif !auth_url.nil? && !auth_url.empty?
      auth_uri = URI.parse(auth_url)
      flint_resp = if auth_uri.path.end_with?('/post_auth')
                     form_pairs = URI.decode_www_form(auth_uri.query || '')
                     form_hash = form_pairs.to_h
                     post_target_str = form_hash.delete('auth_url').to_s
                     raise "Missing auth_url parameter in post_auth URI: #{auth_url}" if post_target_str.empty?

                     form_hash['granted_scope'] = granted_scope_override if granted_scope_override
                     post_target = URI.parse(post_target_str)
                     puts "    [SMART Authorize POST] Submitting PKCE authorization form to #{post_target}"
                     Net::HTTP.post_form(post_target, form_hash)
                   else
                     if granted_scope_override
                       q_pairs = URI.decode_www_form(auth_uri.query || '')
                       q_pairs << ['granted_scope', granted_scope_override]
                       auth_uri.query = URI.encode_www_form(q_pairs)
                     end
                     puts "    [SMART Authorize GET] Requesting PKCE authorization at #{auth_uri.scheme}://#{auth_uri.host}:#{auth_uri.port}#{auth_uri.path}"
                     Net::HTTP.get_response(auth_uri)
                   end

      attest_fail_url = outputs['attest_launch_failed_url'].to_s.strip
      if flint_resp.code.to_i >= 400 && !attest_fail_url.empty?
        puts "    [SMART Invalid Launch Attestation] Flint rejected invalid launch (HTTP #{flint_resp.code}); confirming at #{URI.parse(attest_fail_url).path}"
        rack_resp = INFERNO_RACK_MOCK.get(attest_fail_url)
        unless [200, 302, 303].include?(rack_resp.status)
          raise "Inferno attest_launch_failed_url route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
        end
        next
      end

      unless [302, 303].include?(flint_resp.code.to_i)
        raise "Expected OAuth2 302/303 redirect from Flint, got HTTP #{flint_resp.code}: #{flint_resp.body}"
      end

      callback_location = flint_resp['location'].to_s.strip
      raise 'Flint OAuth2 authorize redirect omitted Location header' if callback_location.empty?

      puts "    [SMART Callback] Delivering authorization code redirect to Inferno #{URI.parse(callback_location).path}"
      rack_resp = INFERNO_RACK_MOCK.get(callback_location)
      unless [200, 302, 303].include?(rack_resp.status)
        raise "Inferno redirect resume route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
      end
    elsif (!continuation_url.nil? && !continuation_url.empty?) || waiting_result.test_id.to_s.include?('invoke_hook')
      puts "    [CRD InvokeHook] Waiting for async CRD hook worker to complete #{waiting_result.test_id} ..."
      crd_deadline = Time.now + 45.0
      while %w[waiting queued running].include?(runs_repo.status_for_test_run(test_run_id).to_s) && Time.now <= crd_deadline
        sleep 0.15
      end
      pending_threads = $active_crd_job_threads_mutex.synchronize do
        copy = $active_crd_job_threads.dup
        $active_crd_job_threads.clear
        copy
      end
      pending_threads.each { |thr| thr.join(15.0) }
      if %w[waiting queued running].include?(runs_repo.status_for_test_run(test_run_id).to_s)
        raise "Timed out waiting for async CRD InvokeHook completion on #{waiting_result.test_id}"
      end
    elsif waiting_result.test_id.to_s.include?('subscription_notification_wait')
      puts "    [PAS Pended Notification] Waiting for async PASSubscriptionTopic rest-hook webhook to resume #{waiting_result.test_id} ..."
      pas_deadline = Time.now + 30.0
      while %w[waiting queued running].include?(runs_repo.status_for_test_run(test_run_id).to_s) && Time.now <= pas_deadline
        sleep 0.15
      end
      if %w[waiting queued running].include?(runs_repo.status_for_test_run(test_run_id).to_s)
        raise "Timed out waiting for async PAS pended rest-hook notification on #{waiting_result.test_id}"
      end
    elsif waiting_result.test_id.to_s.include?('qr_code_scan_test')
      shc_uri = URI.parse("#{flint_fhir_url}/Patient/#{patient_id}/$health-cards.smart-health-card")
      shc_doc = JSON.parse(Net::HTTP.get(shc_uri))
      jws_str = Array(shc_doc['verifiableCredential']).first.to_s
      qr_chunk = "shc:/#{jws_str.chars.map { |c| format('%02d', c.ord - 45) }.join}"
      wait_id = waiting_result.identifier.to_s
      puts "    [SMART Health Cards QR] Submitting numeric SHC QR chunk to /custom/smart_health_cards/post_qr_code?id=#{wait_id}"
      rack_resp = INFERNO_RACK_MOCK.post(
        "/custom/smart_health_cards/post_qr_code?id=#{URI.encode_www_form_component(wait_id)}",
        'CONTENT_TYPE' => 'application/json',
        input: JSON.generate({ qr_code_content: qr_chunk })
      )
      unless [200, 201, 202, 204, 302, 303].include?(rack_resp.status)
        raise "Inferno SHC post_qr_code route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
      end
    elsif waiting_result.test_id.to_s.include?('uds_plus_post_manifest_test')
      wait_id = waiting_result.identifier.to_s
      manifest_json = Net::HTTP.get(URI.parse("#{flint_fhir_url}/$uds-plus-manifest"))
      puts "    [HRSA UDS+] Posting UDS+ Import Manifest Parameters to /custom/uds_plus/postHere?id=#{wait_id}"
      rack_resp = INFERNO_RACK_MOCK.post(
        "/custom/uds_plus/postHere?id=#{URI.encode_www_form_component(wait_id)}",
        'CONTENT_TYPE' => 'application/fhir+json',
        input: manifest_json
      )
      unless [200, 201, 202, 204, 302, 303].include?(rack_resp.status)
        raise "Inferno UDS+ postHere route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
      end
    elsif waiting_result.test_id.to_s.include?('request_receive_post')
      wait_id = waiting_result.identifier.to_s
      hl_uri = URI.parse("#{flint_fhir_url}/Patient/#{patient_id}/$home-lab-report")
      hl_req = Net::HTTP::Get.new(hl_uri)
      hl_req['Accept'] = 'application/fhir+json'
      hl_req['Authorization'] = "Bearer #{bearer_token}" unless bearer_token.to_s.empty?
      hl_resp = Net::HTTP.start(hl_uri.hostname, hl_uri.port, use_ssl: hl_uri.scheme == 'https', read_timeout: 20) do |http|
        http.request(hl_req)
      end
      puts "    [At-Home In-Vitro] Posting home-lab message Bundle to /custom/at_home_test_kit/bundle?id=#{wait_id}"
      rack_resp = INFERNO_RACK_MOCK.post(
        "/custom/at_home_test_kit/bundle?id=#{URI.encode_www_form_component(wait_id)}",
        'CONTENT_TYPE' => 'application/fhir+json',
        input: hl_resp.body.to_s
      )
      unless [200, 201, 202, 204, 302, 303].include?(rack_resp.status)
        raise "Inferno At-Home In-Vitro bundle route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
      end
    elsif (resume_match = waiting_result.result_message.to_s.match(%r{https?://[^\s\)\]]+/resume_pass\?token=[^\s\)\]]+}))
      resume_uri = URI.parse(resume_match[0])
      puts "    [Inferno Attestation] Confirming #{waiting_result.test_id} via #{resume_uri.path}"
      rack_resp = INFERNO_RACK_MOCK.get(resume_uri.request_uri)
      unless [200, 302, 303].include?(rack_resp.status)
        raise "Inferno resume_pass route failed (HTTP #{rack_resp.status}): #{rack_resp.body}"
      end
    else
      raise "Unhandled Inferno waiting state on test #{waiting_result.test_id} (outputs: #{outputs.keys.join(', ')})"
    end
  end
end

def find_group_in_suite(root_runnable, target_suffix)
  target = target_suffix.to_s
  queue = Array(root_runnable.groups).dup
  until queue.empty?
    g = queue.shift
    gid = g.id.to_s
    sid = g.respond_to?(:short_id) ? g.short_id.to_s : ''
    if gid == target || gid.end_with?("-#{target}") || gid.end_with?("_#{target}") || gid.include?(target) || sid == target
      return g
    end

    queue.concat(Array(g.groups))
  end
  nil
end

def execute_inferno_target(
  label:,
  suite_id:,
  flint_base_url:,
  flint_fhir_url:,
  flint_tenant:,
  patient_id:,
  encounter_id:,
  artifacts_dir:,
  bearer_token: '',
  group_suffix: nil,
  runnables: nil,
  suite_options: {},
  inputs: {},
  per_runnable_hooks: {},
  auto_radio_true: false,
  populate_defaults: false
)
  suites_repo   = Inferno::Repositories::TestSuites.new
  tests_repo    = Inferno::Repositories::Tests.new
  sessions_repo = Inferno::Repositories::TestSessions.new
  data_repo     = Inferno::Repositories::SessionData.new
  runs_repo     = Inferno::Repositories::TestRuns.new
  results_repo  = Inferno::Repositories::Results.new
  messages_repo = Inferno::Repositories::Messages.new

  suite = suites_repo.find(suite_id)
  raise "Inferno suite '#{suite_id}' is not registered" if suite.nil?

  target_runnables = if !runnables.nil?
                       Array(runnables)
                     elsif group_suffix.nil?
                       [suite]
                     else
                       found = find_group_in_suite(suite, group_suffix)
                       if found.nil?
                         top_groups = Array(suite.groups).map { |g| "#{g.id} (#{g.title})" }.join(', ')
                         raise "Group '#{group_suffix}' not found in suite '#{suite_id}'. Available top-level groups: #{top_groups}"
                       end
                       [found]
                     end
  raise "No target runnables resolved for suite '#{suite_id}'" if target_runnables.empty?

  declared_opt_ids = Array(suite.respond_to?(:suite_options) ? suite.suite_options : nil).map { |o| o.id.to_sym }
  filtered_options = if declared_opt_ids.empty?
                       suite_options
                     else
                       suite_options.select { |k, _| declared_opt_ids.include?(k.to_sym) }
                     end

  test_session = begin
    sessions_repo.create(
      test_suite_id: suite.id,
      suite_options: filtered_options.map { |k, v| { id: k.to_sym, value: v.to_s } }
    )
  rescue StandardError
    sessions_repo.create(
      test_suite_id: suite.id,
      suite_options: filtered_options.map { |k, v| Inferno::DSL::SuiteOption.new(id: k.to_sym, value: v.to_s) }
    )
  end

  started_at = Time.now
  leaf_results = []

  target_runnables.each_with_index do |runnable, idx|
    available = runnable.available_inputs
    available_list = available.is_a?(Hash) ? available.values : Array(available)
    puts "\n>>> Preparing #{label} [#{idx + 1}/#{target_runnables.size}] (#{runnable.id}) [inputs: #{available_list.map { |ri| "#{ri.name}:#{ri.type}" }.join(', ')}]"

    if populate_defaults
      available_list.each do |ri|
        next if ri.respond_to?(:locked) && ri.locked
        next unless ri.respond_to?(:default) && !ri.default.nil? && !ri.default.to_s.empty?

        if idx.positive?
          existing = begin
            data_repo.load(test_session_id: test_session.id, name: ri.name)
          rescue StandardError
            nil
          end
          next unless existing.nil? || existing.to_s.strip.empty?
        end

        data_repo.save(
          test_session_id: test_session.id,
          name: ri.name,
          value: ri.default.to_s,
          type: ri.type
        )
      end
    end

    if auto_radio_true
      available_list.each do |ri|
        next unless ri.type.to_s == 'radio'
        next if ri.respond_to?(:locked) && ri.locked
        next if inputs.key?(ri.name.to_sym) || inputs.key?(ri.name.to_s)

        data_repo.save(
          test_session_id: test_session.id,
          name: ri.name,
          value: 'true',
          type: ri.type
        )
      end
    end

    inputs.each do |k, v|
      matched = available_list.find { |ri| ri.name.to_s == k.to_s }
      next unless matched

      if idx.positive?
        existing = begin
          data_repo.load(test_session_id: test_session.id, name: matched.name)
        rescue StandardError
          nil
        end
        next unless existing.nil? || existing.to_s.strip.empty?
      end

      val_to_save = v
      if matched.type.to_s == 'oauth_credentials' && v.is_a?(String) && !v.strip.start_with?('{')
        val_to_save = JSON.generate({ access_token: v })
      end

      data_repo.save(
        test_session_id: test_session.id,
        name: matched.name,
        value: val_to_save,
        type: matched.type
      )
    end

    hook_cfg = per_runnable_hooks.find { |pattern, _| runnable.id.to_s.include?(pattern.to_s) }&.last || {}
    if hook_cfg[:before_run]
      hook_cfg[:before_run].call(test_session, data_repo, available_list)
    end

    runnable_key = runnable < Inferno::TestSuite ? :test_suite_id : :test_group_id
    test_run = runs_repo.create(
      test_session_id: test_session.id,
      runnable_key => runnable.id,
      status: 'queued'
    )

    puts ">>> Running #{label} (#{runnable.id}) ..."
    Inferno::Jobs.perform(Inferno::Jobs::ExecuteTestRun, test_run.id, force_synchronous: true)
    drive_waiting_smart_flow!(
      test_session_id: test_session.id,
      test_run_id: test_run.id,
      runs_repo: runs_repo,
      results_repo: results_repo,
      flint_base_url: flint_base_url,
      flint_fhir_url: flint_fhir_url,
      flint_tenant: flint_tenant,
      patient_id: patient_id,
      encounter_id: encounter_id,
      bearer_token: bearer_token,
      granted_scope_override: hook_cfg[:granted_scope],
      expect_heartbeat: hook_cfg.fetch(:expect_heartbeat, false)
    )

    results = runs_repo.results_for_test_run(test_run.id)
    leaf_results.concat(results.select { |r| !r.test_id.nil? && !r.test_id.to_s.empty? })
  end

  elapsed_s = (Time.now - started_at).round(2)
  runnable_summary_id = target_runnables.size == 1 ? target_runnables.first.id : target_runnables.map(&:id).join(',')

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

  artifact_payload = {
    label: label,
    suite_id: suite.id,
    runnable_id: runnable_summary_id,
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

  File.write(File.join(artifacts_dir, "#{label}.json"), JSON.pretty_generate(artifact_payload))
  puts format(
    '<<< %<label>-34s total=%<tot>3d pass=%<pass>3d skip=%<skip>3d omit=%<omit>2d fail=%<fail>2d error=%<err>2d (%<sec>.2fs)',
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
register_jwks_with_flint!(flint_base_url, asym_client_id)
register_jwks_with_flint!(flint_base_url, g10_bulk_client)
register_symmetric_client_with_flint!(flint_base_url, ehr_client_id, ehr_client_secret)
register_symmetric_client_with_flint!(flint_base_url, g10_client_id, g10_client_secret)

common_ctx = {
  flint_base_url: flint_base_url,
  flint_fhir_url: flint_fhir_url,
  flint_tenant: flint_tenant,
  patient_id: launch_patient_id,
  encounter_id: launch_enc_id,
  bearer_token: access_token,
  artifacts_dir: artifacts_dir
}

standalone_public_auth_json = JSON.generate({
  auth_type: 'public',
  use_discovery: 'true',
  auth_request_method: 'GET',
  pkce_support: 'enabled',
  pkce_code_challenge_method: 'S256',
  client_id: 'inferno-standalone-public',
  requested_scopes: 'launch/patient openid fhirUser offline_access patient/*.rs'
})

standalone_asymmetric_auth_json = JSON.generate({
  auth_type: 'asymmetric',
  use_discovery: 'true',
  auth_request_method: 'GET',
  pkce_support: 'enabled',
  pkce_code_challenge_method: 'S256',
  encryption_algorithm: 'ES384',
  client_id: asym_client_id,
  requested_scopes: 'launch/patient openid fhirUser offline_access patient/*.rs'
})

ehr_symmetric_auth_json = JSON.generate({
  auth_type: 'symmetric',
  use_discovery: 'true',
  auth_request_method: 'POST',
  pkce_support: 'enabled',
  pkce_code_challenge_method: 'S256',
  client_id: ehr_client_id,
  client_secret: ehr_client_secret,
  requested_scopes: 'launch launch/encounter openid fhirUser offline_access user/*.cruds'
})

backend_auth_json = JSON.generate({
  auth_type: 'backend_services',
  use_discovery: 'true',
  token_url: "#{flint_base_url}/oauth2/token",
  client_id: client_id,
  encryption_algorithm: 'ES384',
  requested_scopes: 'system/*.rs'
})

introspect_basic_header = "Authorization: Basic #{Base64.strict_encode64("#{ehr_client_id}:#{ehr_client_secret}")}"

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
  **common_ctx,
  label: 'smart_stu2',
  suite_id: 'smart_stu2',
  group_suffix: nil,
  inputs: {
    url: flint_fhir_url,
    standalone_smart_auth_info: standalone_public_auth_json,
    ehr_smart_auth_info: ehr_symmetric_auth_json,
    backend_services_smart_auth_info: backend_auth_json,
    custom_authorization_header: introspect_basic_header
  }
)

runs << execute_inferno_target(
  **common_ctx,
  label: 'smart_stu2_asymmetric',
  suite_id: 'smart_stu2',
  group_suffix: 'smart_full_standalone_launch',
  inputs: {
    url: flint_fhir_url,
    standalone_smart_auth_info: standalone_asymmetric_auth_json
  }
)

runs << execute_inferno_target(
  **common_ctx,
  label: 'bulk_data_v200',
  suite_id: 'bulk_data_v200',
  group_suffix: nil,
  inputs: {
    url: flint_fhir_url,
    bulk_server_url: flint_fhir_url,
    group_id: group_id,
    bulk_timeout: '60',
    smart_auth_info: bulk_auth_json,
    backend_services_auth_info: bulk_auth_json,
    backend_services_smart_auth_info: bulk_auth_json,
    bearer_token: access_token,
    backend_services_client_id: client_id,
    client_id: client_id,
    encryption_algorithm: 'ES384',
    backend_services_requested_scope: 'system/*.read'
  }
)

runs << execute_inferno_target(
  **common_ctx,
  label: 'us_core_v610',
  suite_id: 'us_core_v610',
  group_suffix: 'fhir_api',
  suite_options: {
    smart_app_launch_version: 'smart_app_launch_2'
  },
  inputs: {
    url: flint_fhir_url,
    patient_ids: patient_ids,
    smart_auth_info: us_core_auth_json,
    smart_credentials: access_token
  }
)

if Inferno::Repositories::TestSuites.new.find('us_core_v700')
  runs << execute_inferno_target(
    **common_ctx,
    label: 'us_core_v700',
    suite_id: 'us_core_v700',
    group_suffix: 'fhir_api',
    suite_options: {
      smart_app_launch_version: 'smart_app_launch_2'
    },
    inputs: {
      url: flint_fhir_url,
      patient_ids: patient_ids,
      smart_auth_info: us_core_auth_json,
      smart_credentials: access_token
    }
  )
end

if Inferno::Repositories::TestSuites.new.find('us_core_v800')
  runs << execute_inferno_target(
    **common_ctx,
    label: 'us_core_v800',
    suite_id: 'us_core_v800',
    group_suffix: 'fhir_api',
    suite_options: {
      smart_app_launch_version: 'smart_app_launch_2'
    },
    inputs: {
      url: flint_fhir_url,
      patient_ids: patient_ids,
      smart_auth_info: us_core_auth_json,
      smart_credentials: access_token
    }
  )
end

ipa_suite = Inferno::Repositories::TestSuites.new.all.find do |s|
  s.id.to_s == 'ipa_v100' || s.id.to_s.start_with?('ipa')
end
if ipa_suite && ENV['INFERNO_SKIP_IPA'] != 'true'
  ipa_api_groups = Array(ipa_suite.groups).reject do |g|
    ident = "#{g.id} #{g.title}".downcase
    ident.include?('smart') || ident.include?('launch')
  end
  runs << execute_inferno_target(
    **common_ctx,
    label: ipa_suite.id.to_s,
    suite_id: ipa_suite.id.to_s,
    runnables: ipa_api_groups,
    suite_options: {
      smart_app_launch_version: 'smart_app_launch_2'
    },
    inputs: {
      url: flint_fhir_url,
      patient_id: patient_ids.split(',').first.to_s.strip,
      patient_ids: patient_ids,
      smart_auth_info: us_core_auth_json,
      smart_credentials: access_token,
      access_token: access_token,
      bearer_token: access_token
    }
  )
end

ips_suite = Inferno::Repositories::TestSuites.new.find('ips')
if ips_suite && ENV['INFERNO_SKIP_IPS'] != 'true'
  unless access_token.empty?
    auth_hdr = { 'Authorization' => "Bearer #{access_token}" }
    Array(ips_suite.groups).each do |grp|
      next if !grp.respond_to?(:fhir_client_definitions) || grp.fhir_client_definitions.empty?

      grp.fhir_client do
        url :url
        headers auth_hdr
      end
    end
  end

  ips_summary_uri = URI.parse("#{flint_fhir_url}/Patient/#{launch_patient_id}/$summary")
  ips_summary_req = Net::HTTP::Get.new(ips_summary_uri)
  ips_summary_req['Accept'] = 'application/fhir+json'
  ips_summary_req['Authorization'] = "Bearer #{access_token}" unless access_token.empty?
  ips_summary_resp = Net::HTTP.start(ips_summary_uri.hostname, ips_summary_uri.port, use_ssl: ips_summary_uri.scheme == 'https', read_timeout: 20) do |http|
    http.request(ips_summary_req)
  end
  ips_bundle_json = ips_summary_resp.code.to_i == 200 ? ips_summary_resp.body.to_s : ''

  ips_runnables = Array(ips_suite.groups).reject do |g|
    g.title.to_s.include?('Read and Validate')
  end

  runs << execute_inferno_target(
    **common_ctx,
    label: 'ips_v110',
    suite_id: 'ips',
    runnables: ips_runnables,
    inputs: {
      url: flint_fhir_url,
      patient_id: launch_patient_id,
      bundle_content: ips_bundle_json
    }
  )
end

plannet_suite = Inferno::Repositories::TestSuites.new.find('davinci_plan_net_server_v110')
if plannet_suite && ENV['INFERNO_SKIP_PLANNET'] != 'true'
  plannet_tenant = ENV.fetch('FLINT_PLANNET_TENANT', 'plannet')
  plannet_fhir_url = "#{flint_base_url}/fhir/r4/#{plannet_tenant}"

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: plannet_fhir_url,
    flint_tenant: plannet_tenant,
    label: 'davinci_plan_net_v110',
    suite_id: 'davinci_plan_net_server_v110',
    populate_defaults: true,
    inputs: {
      url: plannet_fhir_url,
      smart_auth_info: us_core_auth_json,
      no_param_search: 'true',
      max_instances: '200',
      max_pages: '20'
    }
  )
end

usdf_suite = Inferno::Repositories::TestSuites.new.find('davinci_us_drug_formulary_v201')
if usdf_suite && ENV['INFERNO_SKIP_USDF'] != 'true'
  formulary_tenant = ENV.fetch('FLINT_FORMULARY_TENANT', 'formulary')
  formulary_fhir_url = "#{flint_base_url}/fhir/r4/#{formulary_tenant}"
  usdf_runnables = Array(usdf_suite.groups).reject do |g|
    g.id.to_s.include?('visual_inspection_and_attestation')
  end

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: formulary_fhir_url,
    flint_tenant: formulary_tenant,
    label: 'davinci_usdf_v201',
    suite_id: 'davinci_us_drug_formulary_v201',
    runnables: usdf_runnables,
    inputs: {
      url: formulary_fhir_url,
      smart_auth_info: us_core_auth_json,
      input_ids: 'loc-usdf-001'
    }
  )
end

c4bb_suite = Inferno::Repositories::TestSuites.new.find('c4bb_v200')
if c4bb_suite && ENV['INFERNO_SKIP_C4BB'] != 'true'
  c4bb_tenant = ENV.fetch('FLINT_C4BB_TENANT', 'c4bb')
  c4bb_fhir_url = "#{flint_base_url}/fhir/r4/#{c4bb_tenant}"
  c4bb_runnables = Array(c4bb_suite.groups).reject do |g|
    g.id.to_s.include?('visual_inspection_and_attestation')
  end
  c4bb_auth_json = JSON.generate({
    auth_type: 'public',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: 'inferno-c4bb-public',
    requested_scopes: [
      'openid fhirUser offline_access launch/patient',
      'patient/ExplanationOfBenefit.read patient/Coverage.read patient/Patient.read',
      'patient/Organization.read patient/Practitioner.read patient/RelatedPerson.read',
      'patient/PractitionerRole.read patient/Location.read',
      'user/ExplanationOfBenefit.read user/Coverage.read user/Patient.read',
      'user/Organization.read user/Practitioner.read user/RelatedPerson.read',
      'user/PractitionerRole.read user/Location.read'
    ].join(' ')
  })

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: c4bb_fhir_url,
    flint_tenant: c4bb_tenant,
    label: 'c4bb_v200',
    suite_id: 'c4bb_v200',
    runnables: c4bb_runnables,
    inputs: {
      url: c4bb_fhir_url,
      smart_auth_info: c4bb_auth_json,
      patient_ids: 'pat-alice-001'
    }
  )
end

pdex_suite = Inferno::Repositories::TestSuites.new.find('pdex_payer_server')
if pdex_suite && ENV['INFERNO_SKIP_PDEX'] != 'true'
  pdex_tenant = ENV.fetch('FLINT_PDEX_TENANT', 'pdex')
  pdex_fhir_url = "#{flint_base_url}/fhir/r4/#{pdex_tenant}"
  pdex_runnables = Array(pdex_suite.groups).reject do |g|
    g.id.to_s.include?('visual_inspection_and_attestation')
  end
  pdex_bulk_auth_json = JSON.generate({
    auth_type: 'backend_services',
    access_token: access_token
  })

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: pdex_fhir_url,
    flint_tenant: pdex_tenant,
    label: 'davinci_pdex_v200',
    suite_id: 'pdex_payer_server',
    runnables: pdex_runnables,
    populate_defaults: true,
    inputs: {
      url: pdex_fhir_url,
      smart_auth_info: us_core_auth_json,
      bulk_server_url: pdex_fhir_url,
      bulk_auth_info: pdex_bulk_auth_json,
      bulk_timeout: '60',
      patient_id: 'pat-alice-001',
      patient_ids: 'pat-alice-001',
      implantable_device_codes: '14106009',
      **FlintTrack7Payloads.pdex_member_match_payloads
    }
  )
end

pas_suite = Inferno::Repositories::TestSuites.new.find('davinci_pas_server_suite_v221')
if pas_suite && ENV['INFERNO_SKIP_PAS'] != 'true'
  pas_tenant = ENV.fetch('FLINT_PAS_TENANT', 'pas')
  pas_fhir_url = "#{flint_base_url}/fhir/r4/#{pas_tenant}"
  pas_notify_token = 'flint-pas-notify-token'
  pas_notify_endpoint = "#{Inferno::Application['base_url']}/custom/subscriptions_r5_backport_r4_server/subscription/channel/notification_listener"
  pas_auth_json = JSON.generate({
    auth_type: 'backend_services',
    access_token: access_token
  })
  pas_runnables = Array(pas_suite.groups).flat_map do |g|
    g.title.to_s.include?('Scenario Support') ? Array(g.groups) : [g]
  end

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: pas_fhir_url,
    flint_tenant: pas_tenant,
    label: 'davinci_pas_v221',
    suite_id: 'davinci_pas_server_suite_v221',
    runnables: pas_runnables,
    inputs: {
      server_endpoint: pas_fhir_url,
      smart_credentials: pas_auth_json,
      access_token: pas_notify_token,
      **FlintTrack7Payloads.pas_v221_inputs(
        notify_endpoint: pas_notify_endpoint,
        notify_token: pas_notify_token
      )
    }
  )
end

udap_suite = Inferno::Repositories::TestSuites.new.find('udap_security')
if udap_suite && ENV['INFERNO_SKIP_UDAP'] != 'true'
  udap_inputs = FlintTrack8Payloads.mint_udap_pki!(
    flint_base_url: flint_base_url,
    flint_fhir_url: flint_fhir_url
  )

  runs << execute_inferno_target(
    **common_ctx,
    label: 'udap_security_v100',
    suite_id: 'udap_security',
    runnables: Array(udap_suite.groups),
    auto_radio_true: true,
    populate_defaults: true,
    inputs: udap_inputs
  )
end

crd_v201_suite = Inferno::Repositories::TestSuites.new.find('crd_server')
if crd_v201_suite && ENV['INFERNO_SKIP_CRD'] != 'true'
  crd_v201_runnables = Array(crd_v201_suite.groups).flat_map do |g|
    g.id.to_s.end_with?('server_hooks') ? Array(g.groups) : [g]
  end

  runs << execute_inferno_target(
    **common_ctx,
    label: 'davinci_crd_v201',
    suite_id: 'crd_server',
    runnables: crd_v201_runnables,
    populate_defaults: true,
    inputs: {
      base_url: flint_base_url,
      **FlintTrack8Payloads.crd_inputs(flint_fhir_url: flint_fhir_url, ig_version: 'v201')
    }
  )
end

crd_v221_suite = Inferno::Repositories::TestSuites.new.find('crd_server_v221')
if crd_v221_suite && ENV['INFERNO_SKIP_CRD'] != 'true'
  crd_v221_runnables = Array(crd_v221_suite.groups).flat_map do |g|
    if g.id.to_s.end_with?('server_hooks') || g.id.to_s.end_with?('specific_coverage_responses')
      Array(g.groups)
    else
      [g]
    end
  end

  runs << execute_inferno_target(
    **common_ctx,
    label: 'davinci_crd_v221',
    suite_id: 'crd_server_v221',
    runnables: crd_v221_runnables,
    populate_defaults: true,
    inputs: {
      base_url: flint_base_url,
      **FlintTrack8Payloads.crd_inputs(flint_fhir_url: flint_fhir_url, ig_version: 'v221')
    }
  )
end

dtr_v201_suite = Inferno::Repositories::TestSuites.new.find('dtr_payer_server')
if dtr_v201_suite && ENV['INFERNO_SKIP_DTR'] != 'true'
  runs << execute_inferno_target(
    **common_ctx,
    label: 'davinci_dtr_v201',
    suite_id: 'dtr_payer_server',
    runnables: Array(dtr_v201_suite.groups),
    populate_defaults: true,
    inputs: {
      url: flint_fhir_url,
      smart_auth_info: us_core_auth_json,
      **FlintTrack8Payloads.dtr_v201_inputs(flint_fhir_url: flint_fhir_url)
    }
  )
end

dtr_v220_suite = Inferno::Repositories::TestSuites.new.find('dtr_payer_server_v220')
if dtr_v220_suite && ENV['INFERNO_SKIP_DTR'] != 'true'
  runs << execute_inferno_target(
    **common_ctx,
    label: 'davinci_dtr_v220',
    suite_id: 'dtr_payer_server_v220',
    runnables: Array(dtr_v220_suite.groups),
    populate_defaults: true,
    inputs: {
      url: flint_fhir_url,
      backend_services_smart_auth_info: backend_auth_json,
      **FlintTrack8Payloads.dtr_v220_inputs(flint_fhir_url: flint_fhir_url)
    }
  )
end

subs_suite = Inferno::Repositories::TestSuites.new.find('subscriptions_r5_backport_r4_server')
if subs_suite && ENV['INFERNO_SKIP_SUBSCRIPTIONS'] != 'true'
  sub_notify_token = 'flint-sub-notify-token'
  sub_channel_endpoint = "#{Inferno::Application['base_url']}/custom/subscriptions_r5_backport_r4_server/subscription/channel/notification_listener"

  build_backport_sub = lambda do |content_mode, with_heartbeat: false|
    ch_exts = [
      {
        url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-timeout',
        valueUnsignedInt: 60
      },
      {
        url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-max-count',
        valuePositiveInt: 20
      }
    ]
    if with_heartbeat
      ch_exts << {
        url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period',
        valueUnsignedInt: 1
      }
    end

    JSON.generate({
      resourceType: 'Subscription',
      meta: {
        profile: ['http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription']
      },
      status: 'requested',
      end: '2099-12-31T12:00:00Z',
      reason: "R4 Topic-Based Subscription (#{content_mode})",
      criteria: 'https://inferno.healthit.gov/suites/custom/subscriptions_r5_backport_r4_client/topics/patient-admission',
      _criteria: {
        extension: [
          {
            url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria',
            valueString: "Encounter?patient=Patient/#{launch_patient_id}"
          }
        ]
      },
      channel: {
        extension: ch_exts,
        type: 'rest-hook',
        header: ["Authorization: Bearer #{sub_notify_token}"],
        endpoint: sub_channel_endpoint,
        payload: 'application/fhir+json',
        _payload: {
          extension: [
            {
              url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content',
              valueCode: content_mode
            }
          ]
        }
      }
    })
  end

  runs << execute_inferno_target(
    **common_ctx,
    label: 'subscriptions_r5_backport',
    suite_id: 'subscriptions_r5_backport_r4_server',
    runnables: Array(subs_suite.groups),
    inputs: {
      url: flint_fhir_url,
      smart_auth_info: us_core_auth_json,
      access_token: sub_notify_token,
      subscription_resource: build_backport_sub.call('full-resource', with_heartbeat: true),
      empty_subscription_resource: build_backport_sub.call('empty', with_heartbeat: false),
      id_only_subscription_resource: build_backport_sub.call('id-only', with_heartbeat: false),
      unsupported_subscription_topic: 'http://example.org/FHIR/SubscriptionTopic/unsupported-topic',
      unsupported_subscription_filter: 'Encounter?unsupported_search_param=123',
      unsupported_subscription_channel_type: 'email',
      unsupported_subscription_channel_endpoint: 'ftp://unsupported.example.org/hook',
      unsupported_subscription_payload_type: 'application/pdf',
      unsupported_subscription_channel_payload_combo: JSON.generate({
        channel: 'websocket',
        payload: 'application/cloudevents+json'
      })
    },
    per_runnable_hooks: {
      'subscriptions_r4_server_workflow' => { expect_heartbeat: true }
    }
  )
end

g10_suite = Inferno::Repositories::TestSuites.new.find('g10_certification')
if g10_suite && ENV['INFERNO_SKIP_G10'] != 'true'
  g10_standalone_scopes = ONCCertificationG10TestKit::ScopeConstants::STANDALONE_SMART_2_SCOPES
  g10_ehr_scopes        = ONCCertificationG10TestKit::ScopeConstants::EHR_SMART_2_SCOPES
  g10_v1_scopes         = ONCCertificationG10TestKit::ScopeConstants::STANDALONE_SMART_1_SCOPES

  g10_standalone_auth_json = JSON.generate({
    auth_type: 'symmetric',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: g10_client_id,
    client_secret: g10_client_secret,
    requested_scopes: g10_standalone_scopes
  })

  g10_ehr_auth_json = JSON.generate({
    auth_type: 'symmetric',
    use_discovery: 'true',
    auth_request_method: 'POST',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: g10_client_id,
    client_secret: g10_client_secret,
    requested_scopes: g10_ehr_scopes
  })

  g10_ehr_patient_auth_json = JSON.generate({
    auth_type: 'symmetric',
    use_discovery: 'true',
    auth_request_method: 'POST',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: g10_client_id,
    client_secret: g10_client_secret,
    requested_scopes: g10_standalone_scopes.sub('launch/patient', 'launch')
  })

  g10_public_auth_json = JSON.generate({
    auth_type: 'public',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: 'inferno-g10-public',
    requested_scopes: g10_standalone_scopes
  })

  g10_asym_auth_json = JSON.generate({
    auth_type: 'asymmetric',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    encryption_algorithm: 'ES384',
    client_id: asym_client_id,
    requested_scopes: g10_standalone_scopes
  })

  g10_v1_auth_json = JSON.generate({
    auth_type: 'symmetric',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: g10_client_id,
    client_secret: g10_client_secret,
    requested_scopes: g10_v1_scopes
  })

  g10_granular_1_scopes = [
    'launch/patient openid fhirUser offline_access',
    *USCoreTestKit::SmartScopesConstants::SMART_GRANULAR_SCOPES_GROUP1['v700']
  ].join(' ')
  g10_granular_2_scopes = [
    'launch/patient openid fhirUser offline_access',
    *USCoreTestKit::SmartScopesConstants::SMART_GRANULAR_SCOPES_GROUP2['v700']
  ].join(' ')

  g10_granular_1_auth_json = JSON.generate({
    auth_type: 'symmetric',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: g10_client_id,
    client_secret: g10_client_secret,
    requested_scopes: g10_granular_1_scopes
  })

  g10_granular_2_auth_json = JSON.generate({
    auth_type: 'symmetric',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: g10_client_id,
    client_secret: g10_client_secret,
    requested_scopes: g10_granular_2_scopes
  })

  g10_granular_selection_auth_json = JSON.generate({
    auth_type: 'symmetric',
    use_discovery: 'true',
    auth_request_method: 'GET',
    pkce_support: 'enabled',
    pkce_code_challenge_method: 'S256',
    client_id: g10_client_id,
    client_secret: g10_client_secret,
    requested_scopes: 'launch/patient openid fhirUser offline_access patient/Condition.rs patient/Observation.rs patient/Patient.rs'
  })

  g10_bulk_auth_json = JSON.generate({
    auth_type: 'backend_services',
    use_discovery: false,
    token_url: "#{flint_base_url}/oauth2/token",
    client_id: g10_bulk_client,
    encryption_algorithm: 'ES384',
    requested_scopes: 'system/*.rs',
    jwks: ONCCertificationG10TestKit::BulkDataJWKSHelper.jwks_json
  })

  g10_runnables = []
  %w[
    g10_smart_standalone_patient_app
    g10_smart_limited_app
    g10_smart_ehr_practitioner_app
    g10_single_patient_us_core_7_api
    multi_patient_api_stu2
  ].each do |gid|
    found = find_group_in_suite(g10_suite, gid)
    g10_runnables << found if found
  end

  group06 = find_group_in_suite(g10_suite, 'Group06')
  if group06
    %w[
      g10_public_standalone_launch_stu2
      g10_smart_invalid_aud
      g10_smart_invalid_token_request_stu2
      g10_smart_invalid_pkce_code_verifier_group
      g10_ehr_patient_launch_stu2
      g10_token_introspection
      g10_asymmetric_launch
      g10_smart_v1_scopes
    ].each do |sub_gid|
      found = Array(group06.groups).find { |g| g.id.to_s.end_with?(sub_gid) }
      g10_runnables << found if found
    end

    fine_grained = Array(group06.groups).find { |g| g.id.to_s.end_with?('g10_us_core_7_smart_fine_grained_scopes') }
    if fine_grained
      Array(fine_grained.groups).each { |fg_child| g10_runnables << fg_child }
    end

    granular_sel = Array(group06.groups).find { |g| g.id.to_s.end_with?('g10_us_core_7_smart_granular_scope_selection') }
    g10_runnables << granular_sel if granular_sel

    tok_rev = Array(group06.groups).find { |g| g.id.to_s.end_with?('g10_token_revocation') }
    g10_runnables << tok_rev if tok_rev
  end

  vis_group = find_group_in_suite(g10_suite, 'g10_visual_inspection_and_attestations')
  g10_runnables << vis_group if vis_group

  g10_introspect_header = "Authorization: Basic #{Base64.strict_encode64("#{g10_client_id}:#{g10_client_secret}")}"

  runs << execute_inferno_target(
    **common_ctx,
    label: 'onc_g10_certification',
    suite_id: 'g10_certification',
    runnables: g10_runnables,
    auto_radio_true: true,
    suite_options: {
      us_core_version: 'us_core_7',
      smart_app_launch_version: 'smart_app_launch_2',
      multi_patient_version: 'multi_patient_api_stu2'
    },
    inputs: {
      url: flint_fhir_url,
      standalone_smart_auth_info: g10_standalone_auth_json,
      expected_resources: 'patient, condition, observation',
      ehr_smart_auth_info: g10_ehr_auth_json,
      additional_patient_ids: '',
      bulk_server_url: flint_fhir_url,
      group_id: group_id,
      bulk_timeout: '60',
      bulk_smart_auth_info: g10_bulk_auth_json,
      public_smart_auth_info: g10_public_auth_json,
      ehr_patient_smart_auth_info: g10_ehr_patient_auth_json,
      custom_authorization_header: g10_introspect_header,
      asymmetric_smart_auth_info: g10_asym_auth_json,
      v1_smart_auth_info: g10_v1_auth_json,
      granular_scopes_1_auth_info: g10_granular_1_auth_json,
      granular_scopes_2_auth_info: g10_granular_2_auth_json,
      granular_scopes_selection_smart_auth_info: g10_granular_selection_auth_json,
      token_revocation_attestation: 'true',
      tls_version_attestation_notes: 'Flint enforces TLS 1.2+ (MinVersion: tls.VersionTLS12) across all HTTPS and ingress listeners.'
    },
    per_runnable_hooks: {
      'g10_smart_limited_app' => {
        granted_scope: 'launch/patient openid fhirUser offline_access patient/Patient.rs patient/Condition.rs patient/Observation.rs'
      },
      'g10_us_core_7_smart_granular_scope_selection' => {
        granted_scope: 'launch/patient openid fhirUser offline_access patient/Patient.rs ' \
                       'patient/Condition.rs?category=http://terminology.hl7.org/CodeSystem/condition-category|problem-list-item ' \
                       'patient/Observation.rs?category=http://terminology.hl7.org/CodeSystem/observation-category|vital-signs'
      },
      'g10_token_revocation' => {
        before_run: lambda do |test_session, data_repo, available_list|
          revoked_info = mint_and_revoke_standalone_token_pair!(
            flint_base_url: flint_base_url,
            flint_fhir_url: flint_fhir_url,
            client_id: g10_client_id,
            client_secret: g10_client_secret,
            patient_id: launch_patient_id
          )
          auth_input = available_list.find { |ri| ri.name.to_s == 'smart_auth_info' }
          if auth_input
            data_repo.save(
              test_session_id: test_session.id,
              name: auth_input.name,
              value: JSON.generate(revoked_info),
              type: auth_input.type
            )
          end
          pat_input = available_list.find { |ri| ri.name.to_s == 'standalone_patient_id' }
          if pat_input
            data_repo.save(
              test_session_id: test_session.id,
              name: pat_input.name,
              value: launch_patient_id,
              type: pat_input.type
            )
          end
        end
      }
    }
  )
end

def fetch_flint_json_bundle!(url_str, bearer_token)
  uri = URI.parse(url_str)
  req = Net::HTTP::Get.new(uri)
  req['Accept'] = 'application/fhir+json'
  req['Authorization'] = "Bearer #{bearer_token}" unless bearer_token.to_s.empty?
  resp = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == 'https', read_timeout: 30) do |http|
    http.request(req)
  end
  unless resp.code.to_i == 200
    raise "Failed to fetch bundle from #{url_str} (HTTP #{resp.code}): #{resp.body}"
  end

  resp.body.to_s
end

ssl_suite = Inferno::Repositories::TestSuites.new.find('smart_scheduling_links')
if ssl_suite && ENV['INFERNO_SKIP_SSL'] != 'true'
  runs << execute_inferno_target(
    **common_ctx,
    label: 'smart_scheduling_links',
    suite_id: 'smart_scheduling_links',
    populate_defaults: true,
    inputs: {
      url: "#{flint_fhir_url}/$bulk-publish",
      max_lines_per_file: '100'
    }
  )
end

shc_suite = Inferno::Repositories::TestSuites.new.find('smart_health_cards')
if shc_suite && ENV['INFERNO_SKIP_SHC'] != 'true'
  runs << execute_inferno_target(
    **common_ctx,
    label: 'smart_health_cards',
    suite_id: 'smart_health_cards',
    inputs: {
      file_download_url: "#{flint_fhir_url}/Patient/#{launch_patient_id}/$health-cards.smart-health-card",
      base_fhir_url: flint_fhir_url,
      patient_id: launch_patient_id
    }
  )
end

shc_vacc_suite = Inferno::Repositories::TestSuites.new.find('shc_vaccination')
if shc_vacc_suite && ENV['INFERNO_SKIP_SHC_VACCINATION'] != 'true'
  runs << execute_inferno_target(
    **common_ctx,
    label: 'shc_vaccination',
    suite_id: 'shc_vaccination',
    inputs: {
      file_download_url: "#{flint_fhir_url}/Patient/#{launch_patient_id}/$health-cards.smart-health-card",
      base_fhir_url: flint_fhir_url,
      patient_id: launch_patient_id
    }
  )
end

smart_udap_suite = Inferno::Repositories::TestSuites.new.find('smart_udap_harmonization')
if smart_udap_suite && ENV['INFERNO_SKIP_SMART_UDAP'] != 'true'
  smart_udap_pki = FlintTrack8Payloads.mint_udap_pki!(
    flint_base_url: flint_base_url,
    flint_fhir_url: flint_fhir_url
  )
  smart_udap_inputs = smart_udap_pki.merge(
    udap_auth_code_flow_registration_scope: 'system/*.rs user/*.rs patient/*.rs launch launch/patient launch/encounter openid fhirUser offline_access',
    udap_authorization_code_request_scopes: 'launch/patient launch/encounter openid fhirUser offline_access patient/Patient.rs patient/Encounter.rs user/Practitioner.rs'
  )

  runs << execute_inferno_target(
    **common_ctx,
    label: 'smart_udap_harmonization',
    suite_id: 'smart_udap_harmonization',
    runnables: Array(smart_udap_suite.groups),
    auto_radio_true: true,
    populate_defaults: true,
    inputs: smart_udap_inputs
  )
end

uds_suite = Inferno::Repositories::TestSuites.new.find('uds_plus')
if uds_suite && ENV['INFERNO_SKIP_UDS_PLUS'] != 'true'
  runs << execute_inferno_target(
    **common_ctx,
    label: 'uds_plus_v200',
    suite_id: 'uds_plus',
    runnables: Array(uds_suite.groups),
    inputs: {
      import_manifest: "#{flint_fhir_url}/$uds-plus-manifest",
      coverage_data: "#{flint_fhir_url}/$uds-plus-resource/coverage",
      diagnosis_data: "#{flint_fhir_url}/$uds-plus-resource/diagnosis",
      encounter_data: "#{flint_fhir_url}/$uds-plus-resource/encounter",
      patient_data: "#{flint_fhir_url}/$uds-plus-resource/patient",
      procedure_data: "#{flint_fhir_url}/$uds-plus-resource/procedure",
      related_person_data: "#{flint_fhir_url}/$uds-plus-resource/related_person",
      immunization_data: "#{flint_fhir_url}/$uds-plus-resource/immunization",
      location_data: "#{flint_fhir_url}/$uds-plus-resource/location",
      allergy_intolerance_data: "#{flint_fhir_url}/$uds-plus-resource/allergy_intolerance",
      adverse_event_data: "#{flint_fhir_url}/$uds-plus-resource/adverse_event",
      income_data: "#{flint_fhir_url}/$uds-plus-resource/income",
      lab_data: "#{flint_fhir_url}/$uds-plus-resource/lab",
      clinical_result_data: "#{flint_fhir_url}/$uds-plus-resource/clinical_result",
      observation_data: "#{flint_fhir_url}/$uds-plus-resource/observation",
      medication_request_data: "#{flint_fhir_url}/$uds-plus-resource/medication_request",
      medication_statement_data: "#{flint_fhir_url}/$uds-plus-resource/medication_statement",
      service_request_data: "#{flint_fhir_url}/$uds-plus-resource/service_request",
      data_urls_data: "#{flint_fhir_url}/$uds-plus-resource/data_urls",
      patient_reporting_parameters_data: "#{flint_fhir_url}/$uds-plus-resource/reporting_parameters"
    }
  )
end

athome_suite = Inferno::Repositories::TestSuites.new.find('at_home_test_kit')
if athome_suite && ENV['INFERNO_SKIP_AT_HOME'] != 'true'
  home_lab_bundle = fetch_flint_json_bundle!("#{flint_fhir_url}/Patient/#{launch_patient_id}/$home-lab-report", access_token)
  runs << execute_inferno_target(
    **common_ctx,
    label: 'at_home_in_vitro_v100',
    suite_id: 'at_home_test_kit',
    runnables: Array(athome_suite.groups),
    inputs: {
      session_id: 'flint-athome-session-1',
      fhir_bundle: home_lab_bundle
    }
  )
end

cpds_access_suite = Inferno::Repositories::TestSuites.new.find('cpds_data_access')
if cpds_access_suite && ENV['INFERNO_SKIP_CPDS'] != 'true'
  cpds_tenant = ENV.fetch('FLINT_CPDS_TENANT', 'cpds')
  cpds_fhir_url = "#{flint_base_url}/fhir/r4/#{cpds_tenant}"

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: cpds_fhir_url,
    flint_tenant: cpds_tenant,
    label: 'cpds_data_access_v101',
    suite_id: 'cpds_data_access',
    inputs: {
      url: cpds_fhir_url,
      patient_ids: 'pat-alice-001',
      implantable_device_codes: '14106009',
      smart_auth_info: us_core_auth_json,
      smart_credentials: us_core_auth_json
    }
  )
end

cpds_report_suite = Inferno::Repositories::TestSuites.new.find('cpds_report_generation')
if cpds_report_suite && ENV['INFERNO_SKIP_CPDS'] != 'true'
  cpds_bundle_json = fetch_flint_json_bundle!("#{flint_fhir_url}/Patient/pat-alice-001/$pathology-exchange-bundle", access_token)
  runs << execute_inferno_target(
    **common_ctx,
    label: 'cpds_report_generation_v101',
    suite_id: 'cpds_report_generation',
    runnables: Array(cpds_report_suite.groups),
    inputs: {
      reports: cpds_bundle_json
    }
  )
end

ccrr_ehr_suite = Inferno::Repositories::TestSuites.new.find('ccrr_ehr')
if ccrr_ehr_suite && ENV['INFERNO_SKIP_CCRR'] != 'true'
  ccrr_tenant = ENV.fetch('FLINT_CCRR_TENANT', 'ccrr')
  ccrr_fhir_url = "#{flint_base_url}/fhir/r4/#{ccrr_tenant}"

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: ccrr_fhir_url,
    flint_tenant: ccrr_tenant,
    label: 'ccrr_ehr_v100',
    suite_id: 'ccrr_ehr',
    inputs: {
      url: ccrr_fhir_url,
      patient_ids: 'pat-ccrr-001',
      implantable_device_codes: '14106009',
      primary_condition_category: 'primary-cancer-condition',
      secondary_condition_category: 'secondary-cancer-condition',
      tnm_distant_metastases_category_code: '21907-1',
      tnm_primary_tumor_category_code: '21905-5',
      tnm_regional_nodes_category_code: '21906-3',
      tnm_stage_group_code: '21908-9',
      smart_auth_info: us_core_auth_json,
      smart_credentials: us_core_auth_json
    }
  )
end

ccrr_report_suite = Inferno::Repositories::TestSuites.new.find('ccrr_v100_report_generation')
if ccrr_report_suite && ENV['INFERNO_SKIP_CCRR'] != 'true'
  ccrr_tenant = ENV.fetch('FLINT_CCRR_TENANT', 'ccrr')
  ccrr_fhir_url = "#{flint_base_url}/fhir/r4/#{ccrr_tenant}"
  ccrr_bundle_json = fetch_flint_json_bundle!("#{ccrr_fhir_url}/Patient/pat-ccrr-001/$cancer-registry-report", access_token)

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: ccrr_fhir_url,
    flint_tenant: ccrr_tenant,
    label: 'ccrr_v100_report_generation',
    suite_id: 'ccrr_v100_report_generation',
    inputs: {
      reports: ccrr_bundle_json
    }
  )
end

usqc_v050_suite = Inferno::Repositories::TestSuites.new.find('us_quality_core_v050')
if usqc_v050_suite && ENV['INFERNO_SKIP_USQC'] != 'true'
  usqc_tenant = ENV.fetch('FLINT_USQC_TENANT', 'usqc')
  usqc_fhir_url = "#{flint_base_url}/fhir/r4/#{usqc_tenant}"

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: usqc_fhir_url,
    flint_tenant: usqc_tenant,
    label: 'us_quality_core_v050',
    suite_id: 'us_quality_core_v050',
    inputs: {
      url: usqc_fhir_url,
      patient_ids: 'pat-alice-001',
      smart_auth_info: us_core_auth_json
    }
  )
end

usqc_v100_suite = Inferno::Repositories::TestSuites.new.find('us_quality_core_v100_ballot')
if usqc_v100_suite && ENV['INFERNO_SKIP_USQC'] != 'true'
  usqc_tenant = ENV.fetch('FLINT_USQC_TENANT', 'usqc')
  usqc_fhir_url = "#{flint_base_url}/fhir/r4/#{usqc_tenant}"

  runs << execute_inferno_target(
    **common_ctx,
    flint_fhir_url: usqc_fhir_url,
    flint_tenant: usqc_tenant,
    label: 'us_quality_core_v100_ballot',
    suite_id: 'us_quality_core_v100_ballot',
    inputs: {
      url: usqc_fhir_url,
      patient_ids: 'pat-alice-001',
      smart_auth_info: us_core_auth_json
    }
  )
end

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
    '  [%<st>s] %<lbl>-34s pass=%<pass>3d  skip=%<skip>3d  omit=%<omit>2d  fail=%<fail>2d  error=%<err>2d',
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
