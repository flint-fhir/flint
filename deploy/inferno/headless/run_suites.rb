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

FileUtils.mkdir_p(File.join(__dir__, 'data'))
$LOAD_PATH.unshift(File.join(__dir__, 'lib')) unless $LOAD_PATH.include?(File.join(__dir__, 'lib'))

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
    status, headers, body = Inferno::Web.app.call(inner_env)
    safe_headers = (headers || {}).to_h
    response_finished.each do |cb|
      if cb.arity.zero?
        cb.call
      else
        cb.call(inner_env, status, safe_headers, nil)
      end
    end
    after_reply.each do |cb|
      if cb.arity.zero?
        cb.call
      else
        cb.call(inner_env, status, safe_headers, nil)
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
    auth_url         = outputs['authorization_url']
    confirmation_url = outputs['confirmation_url']

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
  auto_radio_true: false
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

    if auto_radio_true
      available_list.each do |ri|
        next unless ri.type.to_s == 'radio'
        next if ri.respond_to?(:locked) && ri.locked

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
