#!/usr/bin/env ruby
# frozen_string_literal: true

# Lightweight HL7 FHIR Validator Wrapper HTTP bridge for headless CI runs.
#
# Implements the Inferno `FHIR_RESOURCE_VALIDATOR_URL` `/validate` JSON contract
# by delegating each `filesToValidate` resource to Flint's native StructureDefinition
# & US Core v6.1.0/v7.0.0 `$validate` operation (`POST /{resourceType}/$validate?profile=...`),
# avoiding any multi-GB Java container or external `tx.fhir.org` dependency in CI.
# When an external HL7 Java Validator Wrapper container is running, set
# `FHIR_RESOURCE_VALIDATOR_URL` directly to override this bridge.

require 'json'
require 'net/http'
require 'securerandom'
require 'uri'
require 'webrick'

bind_host = ENV.fetch('VALIDATOR_BRIDGE_HOST', '127.0.0.1')
bind_port = Integer(ENV.fetch('VALIDATOR_BRIDGE_PORT', '14568'))
flint_fhir_url = ENV.fetch('FLINT_FHIR_URL', 'http://127.0.0.1:18080/fhir/r4/demo').chomp('/')
bearer_token = ENV.fetch('FLINT_VALIDATOR_TOKEN', '').strip

SEVERITY_MAP = {
  'fatal' => 'FATAL',
  'error' => 'ERROR',
  'warning' => 'WARNING',
  'information' => 'INFORMATION'
}.freeze

def call_flint_validate_outcome(flint_fhir_url, bearer_token, raw_content, profiles)
  parsed = JSON.parse(raw_content)
  res_type = parsed['resourceType'].to_s.strip
  endpoint = if res_type.empty?
               "#{flint_fhir_url}/$validate"
             else
               "#{flint_fhir_url}/#{URI.encode_www_form_component(res_type)}/$validate"
             end

  if profiles.is_a?(Array) && !profiles.empty?
    query = URI.encode_www_form(profiles.map { |p| ['profile', p.to_s] })
    endpoint = "#{endpoint}?#{query}"
  end

  uri = URI.parse(endpoint)
  req = Net::HTTP::Post.new(uri)
  req['Content-Type'] = 'application/fhir+json'
  req['Accept'] = 'application/fhir+json'
  req['Authorization'] = "Bearer #{bearer_token}" unless bearer_token.empty?
  req.body = raw_content

  resp = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == 'https', read_timeout: 30) do |http|
    http.request(req)
  end

  JSON.parse(resp.body)
end

def validate_against_flint(flint_fhir_url, bearer_token, raw_content, profiles)
  outcome = call_flint_validate_outcome(flint_fhir_url, bearer_token, raw_content, profiles)
  Array(outcome['issue']).filter_map do |iss|
    next unless iss.is_a?(Hash)

    sev = SEVERITY_MAP.fetch(iss['severity'].to_s.downcase, 'INFORMATION')
    msg = iss['diagnostics'].to_s
    msg = iss.dig('details', 'text').to_s if msg.empty?
    next if msg.empty?

    loc = Array(iss['expression']).first || Array(iss['location']).first || 'Resource'
    {
      'level' => sev,
      'location' => loc.to_s,
      'message' => msg
    }
  end
rescue JSON::ParserError => e
  [{ 'level' => 'ERROR', 'location' => 'Resource', 'message' => "Malformed JSON: #{e.message}" }]
rescue StandardError => e
  [{ 'level' => 'WARNING', 'location' => 'ValidatorBridge', 'message' => "Validator bridge warning: #{e.message}" }]
end

server = WEBrick::HTTPServer.new(
  BindAddress: bind_host,
  Port: bind_port,
  Logger: WEBrick::Log.new($stderr, WEBrick::Log::WARN),
  AccessLog: []
)

server.mount_proc '/health' do |_req, res|
  res.status = 200
  res['Content-Type'] = 'application/json'
  res.body = JSON.generate({ status: 'ok', backend: flint_fhir_url })
end

server.mount_proc '/version' do |_req, res|
  res.status = 200
  res['Content-Type'] = 'application/json'
  res.body = JSON.generate({
    'org.hl7.fhir.validation' => '6.5.0',
    'inferno-framework/fhir-validator-wrapper' => '2.2.1'
  })
end

server.mount_proc '/validator/version' do |_req, res|
  res.status = 200
  res['Content-Type'] = 'text/plain'
  res.body = '6.5.0'
end

server.mount_proc '/igs' do |_req, res|
  res.status = 200
  res['Content-Type'] = 'application/json'
  res.body = JSON.generate({
    'id' => 'hl7.fhir.us.core',
    'version' => '6.1.0',
    'profiles' => {}
  })
end

server.mount_proc '/validate' do |req, res|
  payload = req.body.to_s.empty? ? {} : JSON.parse(req.body)
  if payload.is_a?(Hash) && payload.key?('filesToValidate')
    ctx = payload['validationContext'] || payload['cliContext'] || {}
    profiles = Array(ctx['profiles'])
    session_id = (payload['sessionId'] || ctx['sessionId']).to_s.strip
    session_id = SecureRandom.uuid if session_id.empty?
    files = Array(payload['filesToValidate'])

    outcomes = files.map do |file|
      file_name = file['fileName'] || 'resource.json'
      file_type = file['fileType'] || 'json'
      content = file['fileContent'].to_s
      {
        'fileInfo' => {
          'fileName' => file_name,
          'fileType' => file_type
        },
        'issues' => validate_against_flint(flint_fhir_url, bearer_token, content, profiles)
      }
    end

    res.status = 200
    res['Content-Type'] = 'application/json'
    res.body = JSON.generate({
      'sessionId' => session_id,
      'outcomes' => outcomes
    })
  else
    query_params = URI.decode_www_form(req.query_string.to_s)
    profiles = query_params.select { |k, _| k == 'profile' }.flat_map { |_, v| v.to_s.split(',') }.reject(&:empty?)
    outcome = call_flint_validate_outcome(flint_fhir_url, bearer_token, req.body.to_s, profiles)
    res.status = 200
    res['Content-Type'] = 'application/fhir+json'
    res.body = JSON.generate(outcome)
  end
end

trap('INT') { server.shutdown }
trap('TERM') { server.shutdown }

server.start
