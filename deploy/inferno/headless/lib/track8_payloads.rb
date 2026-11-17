# frozen_string_literal: true

require 'json'
require 'net/http'
require 'openssl'
require 'securerandom'
require 'uri'

# Deterministic HL7 UDAP Security v1.0.0, Da Vinci CRD v2.0.1/v2.2.1, and
# Da Vinci DTR v2.0.1/v2.2.0 payloads for Flint's headless HL7 Inferno harness.
module FlintTrack8Payloads
  module_function

  def mint_udap_pki!(flint_base_url:, flint_fhir_url:)
    now = Time.now.utc
    ca_key = OpenSSL::PKey::RSA.new(2048)
    ca_cert = OpenSSL::X509::Certificate.new
    ca_cert.version = 2
    ca_cert.serial = OpenSSL::BN.new(SecureRandom.random_number(1 << 62))
    ca_name = OpenSSL::X509::Name.parse('/C=US/O=Flint Health Interoperability/CN=Flint Inferno UDAP Client Root CA')
    ca_cert.subject = ca_name
    ca_cert.issuer = ca_name
    ca_cert.public_key = ca_key.public_key
    ca_cert.not_before = now - 3600
    ca_cert.not_after = now + (365 * 24 * 3600)

    ef = OpenSSL::X509::ExtensionFactory.new
    ef.subject_certificate = ca_cert
    ef.issuer_certificate = ca_cert
    ca_cert.add_extension(ef.create_extension('basicConstraints', 'CA:TRUE', true))
    ca_cert.add_extension(ef.create_extension('keyUsage', 'keyCertSign,cRLSign', true))
    ca_cert.add_extension(ef.create_extension('subjectKeyIdentifier', 'hash', false))
    ca_cert.add_extension(ef.create_extension('authorityKeyIdentifier', 'keyid:always', false))
    ca_cert.sign(ca_key, OpenSSL::Digest::SHA256.new)

    mint_leaf = lambda do |cn, san_uri|
      leaf_key = OpenSSL::PKey::RSA.new(2048)
      leaf_cert = OpenSSL::X509::Certificate.new
      leaf_cert.version = 2
      leaf_cert.serial = OpenSSL::BN.new(SecureRandom.random_number(1 << 62))
      leaf_cert.subject = OpenSSL::X509::Name.parse("/C=US/O=Flint Health Interoperability/CN=#{cn}")
      leaf_cert.issuer = ca_cert.subject
      leaf_cert.public_key = leaf_key.public_key
      leaf_cert.not_before = now - 3600
      leaf_cert.not_after = now + (90 * 24 * 3600)

      lef = OpenSSL::X509::ExtensionFactory.new
      lef.subject_certificate = leaf_cert
      lef.issuer_certificate = ca_cert
      leaf_cert.add_extension(lef.create_extension('basicConstraints', 'CA:FALSE', true))
      leaf_cert.add_extension(lef.create_extension('keyUsage', 'digitalSignature,keyEncipherment', true))
      leaf_cert.add_extension(lef.create_extension('extendedKeyUsage', 'clientAuth', false))
      leaf_cert.add_extension(lef.create_extension('subjectAltName', "URI:#{san_uri}", false))
      leaf_cert.add_extension(lef.create_extension('subjectKeyIdentifier', 'hash', false))
      leaf_cert.add_extension(lef.create_extension('authorityKeyIdentifier', 'keyid:always', false))
      leaf_cert.sign(ca_key, OpenSSL::Digest::SHA256.new)
      [leaf_key, leaf_cert]
    end

    ac_iss = 'https://inferno.flint.local/udap/ac'
    cc_iss = 'https://inferno.flint.local/udap/cc'
    ac_key, ac_cert = mint_leaf.call('Inferno UDAP AuthCode Client', ac_iss)
    cc_key, cc_cert = mint_leaf.call('Inferno UDAP ClientCreds Client', cc_iss)

    reg_uri = URI.parse("#{flint_base_url}/oauth2/udap-trust-anchor")
    reg_req = Net::HTTP::Post.new(reg_uri)
    reg_req['Content-Type'] = 'application/json'
    reg_req.body = JSON.generate({ cert_pem: ca_cert.to_pem })
    reg_resp = Net::HTTP.start(reg_uri.hostname, reg_uri.port, use_ssl: reg_uri.scheme == 'https', read_timeout: 15) do |http|
      http.request(reg_req)
    end
    unless reg_resp.code.to_i.between?(200, 299)
      raise "Failed to register UDAP trust anchor at #{reg_uri} (HTTP #{reg_resp.code}): #{reg_resp.body}"
    end

    srv_ca_uri = URI.parse("#{flint_base_url}/oauth2/udap-ca.pem")
    srv_ca_resp = Net::HTTP.get_response(srv_ca_uri)
    unless srv_ca_resp.code.to_i == 200
      raise "Failed to fetch Flint UDAP server Root CA PEM at #{srv_ca_uri} (HTTP #{srv_ca_resp.code})"
    end

    {
      udap_fhir_base_url: flint_fhir_url,
      udap_server_trust_anchor_certs: srv_ca_resp.body.to_s,
      udap_auth_code_flow_client_registration_status: 'new',
      udap_auth_code_flow_client_cert_pem: ac_cert.to_pem,
      udap_auth_code_flow_client_private_key: ac_key.to_pem,
      udap_auth_code_flow_cert_iss: ac_iss,
      udap_auth_code_flow_registration_scope: 'openid fhirUser launch/patient patient/Patient.rs',
      udap_authorization_code_request_scopes: 'openid fhirUser launch/patient patient/Patient.rs',
      udap_authorization_code_request_aud: JSON.generate(['include_aud']),
      udap_client_credentials_flow_client_registration_status: 'new',
      udap_client_credentials_flow_client_cert_pem: cc_cert.to_pem,
      udap_client_credentials_flow_client_private_key: cc_key.to_pem,
      udap_cert_iss_client_creds_flow: cc_iss,
      udap_client_credentials_flow_registration_scope: 'system/Patient.rs system/*.rs',
      attestation_notes: 'Flint enforces TLS 1.2+, X.509 trust anchor validation, CRL distribution, ' \
                         'RFC 7523 jti replay protection, and UDAP v1.0.0 DCR & JWT client authentication.'
    }
  end

  def us_core_patient(pat_id, family: 'Rivera', given: 'Alice')
    {
      resourceType: 'Patient',
      id: pat_id,
      meta: {
        profile: [
          'http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient',
          'http://hl7.org/fhir/us/davinci-crd/StructureDefinition/profile-patient'
        ]
      },
      identifier: [
        {
          use: 'usual',
          type: {
            coding: [
              {
                system: 'http://terminology.hl7.org/CodeSystem/v2-0203',
                code: 'MR',
                display: 'Medical Record Number'
              }
            ]
          },
          system: 'http://hospital.smarthealthit.org',
          value: "MRN-#{pat_id}"
        }
      ],
      active: true,
      name: [{ use: 'official', family: family, given: [given] }],
      gender: 'female',
      birthDate: '1985-04-12'
    }
  end

  def crd_coverage(cov_id, pat_id, status: 'active')
    {
      resourceType: 'Coverage',
      id: cov_id,
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-crd/StructureDefinition/profile-coverage']
      },
      identifier: [{ system: 'http://flint.health/payer/member', value: "MEM-#{pat_id}" }],
      status: status,
      type: {
        coding: [
          {
            system: 'http://terminology.hl7.org/CodeSystem/v3-ActCode',
            code: 'HIP',
            display: 'health insurance plan policy'
          }
        ]
      },
      subscriberId: "SUB-#{pat_id}",
      beneficiary: { reference: "Patient/#{pat_id}" },
      relationship: {
        coding: [
          {
            system: 'http://terminology.hl7.org/CodeSystem/subscriber-relationship',
            code: 'self',
            display: 'Self'
          }
        ]
      },
      period: { start: '2024-01-01', end: '2030-12-31' },
      payor: [{ reference: 'Organization/org-payer-001', display: 'Flint Health Plan' }]
    }
  end

  def crd_encounter(enc_id, pat_id)
    {
      resourceType: 'Encounter',
      id: enc_id,
      meta: {
        profile: [
          'http://hl7.org/fhir/us/core/StructureDefinition/us-core-encounter',
          'http://hl7.org/fhir/us/davinci-crd/StructureDefinition/profile-encounter'
        ]
      },
      status: 'in-progress',
      class: {
        system: 'http://terminology.hl7.org/CodeSystem/v3-ActCode',
        code: 'AMB',
        display: 'ambulatory'
      },
      type: [
        {
          coding: [
            {
              system: 'http://www.ama-assn.org/go/cpt',
              code: '99213',
              display: 'Office or other outpatient visit'
            }
          ]
        }
      ],
      subject: { reference: "Patient/#{pat_id}" },
      participant: [
        {
          individual: { reference: 'Practitioner/prac-alice-001' }
        }
      ],
      period: { start: '2026-01-15T09:00:00Z' },
      serviceProvider: { reference: 'Organization/org-payer-001' }
    }
  end

  def crd_service_request(sr_id, pat_id)
    {
      resourceType: 'ServiceRequest',
      id: sr_id,
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-crd/StructureDefinition/profile-servicerequest']
      },
      status: 'draft',
      intent: 'order',
      category: [
        {
          coding: [
            {
              system: 'http://snomed.info/sct',
              code: '387713003',
              display: 'Surgical procedure'
            }
          ]
        }
      ],
      code: {
        coding: [
          {
            system: 'http://www.ama-assn.org/go/cpt',
            code: '70551',
            display: 'MRI Brain without contrast'
          }
        ]
      },
      subject: { reference: "Patient/#{pat_id}" },
      authoredOn: '2026-01-15T09:30:00Z',
      requester: { reference: 'Practitioner/prac-alice-001' },
      performer: [{ reference: 'Practitioner/prac-alice-001' }]
    }
  end

  def crd_device_request(dr_id, pat_id)
    {
      resourceType: 'DeviceRequest',
      id: dr_id,
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-crd/StructureDefinition/profile-devicerequest']
      },
      status: 'draft',
      intent: 'original-order',
      codeCodeableConcept: {
        coding: [
          {
            system: 'https://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets',
            code: 'E0424',
            display: 'Stationary compressed gaseous oxygen system, rental'
          }
        ]
      },
      subject: { reference: "Patient/#{pat_id}" },
      authoredOn: '2026-01-15T10:00:00Z',
      requester: { reference: 'Practitioner/prac-alice-001' },
      performer: { reference: 'Practitioner/prac-alice-001' }
    }
  end

  def crd_appointment(appt_id, pat_id)
    {
      resourceType: 'Appointment',
      id: appt_id,
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-crd/StructureDefinition/profile-appointment-no-order']
      },
      status: 'proposed',
      serviceCategory: [
        {
          coding: [
            {
              system: 'http://terminology.hl7.org/CodeSystem/service-category',
              code: '17',
              display: 'General Practice'
            }
          ]
        }
      ],
      serviceType: [
        {
          coding: [
            {
              system: 'http://www.ama-assn.org/go/cpt',
              code: '99214',
              display: 'Outpatient consultation'
            }
          ]
        }
      ],
      start: '2026-06-01T14:00:00Z',
      end: '2026-06-01T14:30:00Z',
      participant: [
        {
          actor: { reference: "Patient/#{pat_id}" },
          status: 'accepted'
        },
        {
          actor: { reference: 'Practitioner/prac-alice-001' },
          status: 'accepted'
        }
      ]
    }
  end

  def crd_fhir_auth(pat_id)
    {
      access_token: 'flint-mock-ehr-token',
      token_type: 'Bearer',
      expires_in: 300,
      scope: 'patient/Patient.read patient/Coverage.read user/Appointment.read user/Encounter.read user/ServiceRequest.read user/DeviceRequest.read',
      subject: 'cds-service4',
      patient: pat_id
    }
  end

  def crd_coverage_searchset(cov_id, pat_id)
    {
      resourceType: 'Bundle',
      type: 'searchset',
      total: 1,
      entry: [
        {
          fullUrl: "http://example.org/fhir/Coverage/#{cov_id}",
          resource: crd_coverage(cov_id, pat_id),
          search: { mode: 'match' }
        }
      ]
    }
  end

  def crd_inputs(flint_fhir_url:, ig_version: 'v221')
    pat_main = 'pat-alice-001'
    pat_cond = 'pat-cond-v221'
    enc_id   = 'enc-alice-001'

    prac = {
      resourceType: 'Practitioner',
      id: 'prac-alice-001',
      meta: { profile: ['http://hl7.org/fhir/us/core/StructureDefinition/us-core-practitioner'] },
      identifier: [{ system: 'http://hl7.org/fhir/sid/us-npi', value: '9941339108' }],
      active: true,
      name: [{ family: 'House', given: ['Gregory'], prefix: ['Dr.'] }]
    }
    org = {
      resourceType: 'Organization',
      id: 'org-payer-001',
      meta: { profile: ['http://hl7.org/fhir/us/core/StructureDefinition/us-core-organization'] },
      identifier: [{ system: 'http://hl7.org/fhir/sid/us-npi', value: '1234567893' }],
      active: true,
      name: 'Flint Health Plan'
    }
    loc = {
      resourceType: 'Location',
      id: 'loc-crd-1',
      meta: { profile: ['http://hl7.org/fhir/us/core/StructureDefinition/us-core-location'] },
      status: 'active',
      name: 'Flint Outpatient Diagnostic Center',
      address: { line: ['100 Health Way'], city: 'Boston', state: 'MA', postalCode: '02115' }
    }

    mock_ehr = {
      resourceType: 'Bundle',
      type: 'collection',
      entry: [
        us_core_patient(pat_main),
        us_core_patient(pat_cond, family: 'Chen', given: 'Marcus'),
        us_core_patient('pat-tech-error', family: 'Error', given: 'Tech'),
        us_core_patient('pat-no-coverage', family: 'Uncovered', given: 'Dana'),
        us_core_patient('pat-inactive-coverage', family: 'Lapsed', given: 'Evan'),
        crd_encounter(enc_id, pat_main),
        prac,
        org,
        loc,
        crd_coverage('cov-crd-1', pat_main),
        crd_coverage('cov-crd-cond', pat_cond),
        crd_service_request('sr-crd-1', pat_main),
        crd_service_request('sr-crd-dispatch-1', pat_main),
        crd_device_request('dr-crd-cond-1', pat_cond),
        crd_appointment('appt-crd-1', pat_main)
      ].map { |res| { resource: res } }
    }

    std_prefetch = {
      patient: us_core_patient(pat_main),
      encounter: crd_encounter(enc_id, pat_main),
      coverage: crd_coverage_searchset('cov-crd-1', pat_main)
    }

    cfg_ext = {
      'davinci-crd.configuration' => {
        'coverage-info' => true,
        'external-reference' => true,
        'instructions' => true
      }
    }

    appt_req = {
      hook: 'appointment-book',
      hookInstance: SecureRandom.uuid,
      fhirServer: flint_fhir_url,
      fhirAuthorization: crd_fhir_auth(pat_main),
      extension: cfg_ext,
      context: {
        userId: 'Practitioner/prac-alice-001',
        patientId: pat_main,
        encounterId: enc_id,
        appointments: {
          resourceType: 'Bundle',
          type: 'collection',
          entry: [{ resource: crd_appointment('appt-crd-1', pat_main) }]
        }
      },
      prefetch: std_prefetch
    }

    enc_start_req = {
      hook: 'encounter-start',
      hookInstance: SecureRandom.uuid,
      fhirServer: flint_fhir_url,
      fhirAuthorization: crd_fhir_auth(pat_main),
      extension: cfg_ext,
      context: {
        userId: 'Practitioner/prac-alice-001',
        patientId: pat_main,
        encounterId: enc_id
      },
      prefetch: std_prefetch
    }

    enc_discharge_req = {
      hook: 'encounter-discharge',
      hookInstance: SecureRandom.uuid,
      fhirServer: flint_fhir_url,
      fhirAuthorization: crd_fhir_auth(pat_main),
      extension: cfg_ext,
      context: {
        userId: 'Practitioner/prac-alice-001',
        patientId: pat_main,
        encounterId: enc_id
      },
      prefetch: std_prefetch
    }

    order_select_req = {
      hook: 'order-select',
      hookInstance: SecureRandom.uuid,
      fhirServer: flint_fhir_url,
      fhirAuthorization: crd_fhir_auth(pat_main),
      extension: cfg_ext,
      context: {
        userId: 'Practitioner/prac-alice-001',
        patientId: pat_main,
        encounterId: enc_id,
        selections: ['ServiceRequest/sr-crd-1'],
        draftOrders: {
          resourceType: 'Bundle',
          type: 'collection',
          entry: [{ resource: crd_service_request('sr-crd-1', pat_main) }]
        }
      },
      prefetch: std_prefetch
    }

    dispatch_ctx = if ig_version == 'v201'
                     {
                       patientId: pat_main,
                       order: 'ServiceRequest/sr-crd-dispatch-1',
                       performer: 'Practitioner/prac-alice-001'
                     }
                   else
                     {
                       patientId: pat_main,
                       dispatchedOrders: ['ServiceRequest/sr-crd-dispatch-1'],
                       performer: 'Practitioner/prac-alice-001'
                     }
                   end

    order_dispatch_req = {
      hook: 'order-dispatch',
      hookInstance: SecureRandom.uuid,
      fhirServer: flint_fhir_url,
      fhirAuthorization: crd_fhir_auth(pat_main),
      extension: cfg_ext,
      context: dispatch_ctx,
      prefetch: {
        patient: us_core_patient(pat_main),
        coverage: crd_coverage_searchset('cov-crd-1', pat_main)
      }
    }

    order_sign_req_covered = {
      hook: 'order-sign',
      hookInstance: SecureRandom.uuid,
      fhirServer: flint_fhir_url,
      fhirAuthorization: crd_fhir_auth(pat_main),
      extension: cfg_ext,
      context: {
        userId: 'Practitioner/prac-alice-001',
        patientId: pat_main,
        encounterId: enc_id,
        draftOrders: {
          resourceType: 'Bundle',
          type: 'collection',
          entry: [{ resource: crd_service_request('sr-crd-1', pat_main) }]
        }
      },
      prefetch: std_prefetch
    }

    # Intentionally omits `extension` (`davinci-crd.configuration`) and `ext-billing-options`
    # while triggering the `conditional` coverage scenario + remaining MS sub-extensions.
    order_sign_req_conditional = {
      hook: 'order-sign',
      hookInstance: SecureRandom.uuid,
      fhirServer: flint_fhir_url,
      fhirAuthorization: crd_fhir_auth(pat_cond),
      context: {
        userId: 'Practitioner/prac-alice-001',
        patientId: pat_cond,
        draftOrders: {
          resourceType: 'Bundle',
          type: 'collection',
          entry: [{ resource: crd_device_request('dr-crd-cond-1', pat_cond) }]
        }
      },
      prefetch: {
        patient: us_core_patient(pat_cond, family: 'Chen', given: 'Marcus'),
        coverage: crd_coverage_searchset('cov-crd-cond', pat_cond)
      }
    }

    build_scenario_req = lambda do |scenario_pat_id, include_patient_prefetch: true|
      pf = include_patient_prefetch ? { patient: us_core_patient(scenario_pat_id) } : {}
      {
        hook: 'order-sign',
        hookInstance: SecureRandom.uuid,
        fhirServer: flint_fhir_url,
        fhirAuthorization: crd_fhir_auth(scenario_pat_id),
        context: {
          userId: 'Practitioner/prac-alice-001',
          patientId: scenario_pat_id,
          draftOrders: {
            resourceType: 'Bundle',
            type: 'collection',
            entry: [{ resource: crd_service_request("sr-#{scenario_pat_id}", scenario_pat_id) }]
          }
        },
        prefetch: pf
      }
    end

    out = {
      encryption_method: 'ES384',
      authentication_required: 'no',
      manual_continuation: 'no',
      crd_discovery_service_ignore_list: 'flint-hypertension-care-gap,flint-diabetes-a1c-care-gap,flint-medication-allergy-check',
      mock_ehr_bundle: JSON.generate(mock_ehr),
      any_hook_request_body: JSON.generate(order_sign_req_covered),
      appointment_book_request_bodies: JSON.generate([appt_req]),
      encounter_start_request_bodies: JSON.generate([enc_start_req]),
      encounter_discharge_request_bodies: JSON.generate([enc_discharge_req]),
      order_select_request_bodies: JSON.generate([order_select_req]),
      order_dispatch_request_bodies: JSON.generate([order_dispatch_req]),
      order_sign_request_bodies: JSON.generate([order_sign_req_covered, order_sign_req_conditional])
    }

    if ig_version == 'v221'
      out[:technical_issues_request_body] = JSON.generate(build_scenario_req.call('pat-tech-error'))
      out[:no_member_found_request_body] = JSON.generate(build_scenario_req.call('pat-unknown-member', include_patient_prefetch: false))
      out[:coverage_not_found_request_body] = JSON.generate(build_scenario_req.call('pat-no-coverage'))
      out[:no_active_coverage_request_body] = JSON.generate(build_scenario_req.call('pat-inactive-coverage'))
    end

    out
  end

  def dtr_v201_inputs(flint_fhir_url:)
    cov = crd_coverage('cov-dtr-1', 'pat-alice-001')
    order = crd_device_request('dr-dtr-1', 'pat-alice-001')

    static_req = {
      resourceType: 'Parameters',
      meta: { profile: ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-input-parameters'] },
      parameter: [
        { name: 'coverage', resource: cov },
        { name: 'order', resource: order },
        { name: 'questionnaire', valueCanonical: 'http://flint.health/fhir/Questionnaire/dtr-home-oxygen|2.2.0' }
      ]
    }

    adapt_req = {
      resourceType: 'Parameters',
      meta: { profile: ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-input-parameters'] },
      parameter: [
        { name: 'coverage', resource: cov },
        { name: 'order', resource: order },
        { name: 'questionnaire', valueCanonical: 'http://flint.health/fhir/Questionnaire/dtr-adapt-v201|1.0.0' }
      ]
    }

    build_nq_qr = lambda do |qr_id, items|
      {
        resourceType: 'QuestionnaireResponse',
        id: qr_id,
        meta: {
          profile: [
            'http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse-adapt',
            'http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaireresponse-adapt'
          ]
        },
        contained: [
          {
            resourceType: 'Questionnaire',
            id: 'adapt-q-1',
            meta: { profile: ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaire-adapt'] },
            extension: [
              {
                url: 'http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaire-questionnaireAdaptive',
                valueUrl: flint_fhir_url
              }
            ],
            url: 'http://flint.health/fhir/Questionnaire/dtr-adapt-v201',
            version: '1.0.0',
            derivedFrom: ['http://flint.health/fhir/Questionnaire/dtr-adapt-v201|1.0.0'],
            status: 'active',
            subjectType: ['Patient'],
            item: items.map { |it| { linkId: it[:linkId], text: it[:text], type: 'string', required: true } }
          }
        ],
        questionnaire: '#adapt-q-1',
        status: 'in-progress',
        subject: { reference: 'Patient/pat-alice-001' },
        authored: '2026-01-15T10:05:00Z',
        item: items.map do |it|
          {
            linkId: it[:linkId],
            text: it[:text],
            answer: [{ valueString: it[:answer] }]
          }
        end
      }
    end

    nq_step1 = build_nq_qr.call('dtr-qr-v201-step1', [
                                  { linkId: 'q1', text: 'Primary Oxygen Saturation Reading', answer: '86%' }
                                ])
    nq_step2 = build_nq_qr.call('dtr-qr-v201-complete', [
                                  { linkId: 'q1', text: 'Primary Oxygen Saturation Reading', answer: '86%' },
                                  { linkId: 'q2', text: 'Confirmed Hypoxemia Diagnosis Details', answer: 'Chronic hypoxemic respiratory failure' }
                                ])

    {
      retrieval_method: 'Both',
      initial_static_questionnaire_request: JSON.generate(static_req),
      initial_adaptive_questionnaire_request: JSON.generate(adapt_req),
      next_question_requests: JSON.generate([nq_step1, nq_step2])
    }
  end

  def dtr_v220_inputs(flint_fhir_url:)
    cov = crd_coverage('cov-dtr-1', 'pat-alice-001')
    order = crd_device_request('dr-dtr-1', 'pat-alice-001')

    # Three distinct $questionnaire-package requests exercising each input slice independently:
    #   [1] coverage + questionnaire ONLY
    #   [2] coverage + context ONLY (triggers adaptive Questionnaire package)
    #   [3] coverage + order ONLY (DeviceRequest, satisfies order type + qr-context)
    qp_by_questionnaire = {
      resourceType: 'Parameters',
      meta: { profile: ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-input-parameters'] },
      parameter: [
        { name: 'coverage', resource: cov },
        { name: 'questionnaire', valueCanonical: 'http://flint.health/fhir/Questionnaire/dtr-home-oxygen|2.2.0' }
      ]
    }
    qp_by_context = {
      resourceType: 'Parameters',
      meta: { profile: ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-input-parameters'] },
      parameter: [
        { name: 'coverage', resource: cov },
        { name: 'context', valueString: 'CRD-ASSERT-ADAPT-OXYGEN-220' }
      ]
    }
    qp_by_order = {
      resourceType: 'Parameters',
      meta: { profile: ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-qpackage-input-parameters'] },
      parameter: [
        { name: 'coverage', resource: cov },
        { name: 'order', resource: order }
      ]
    }

    qr_template = {
      resourceType: 'QuestionnaireResponse',
      id: 'dtr-qr-template-v220',
      meta: {
        profile: [
          'http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse-adapt',
          'http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaireresponse-adapt'
        ]
      },
      contained: [
        {
          resourceType: 'Questionnaire',
          id: 'adapt-q-1',
          meta: { profile: ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaire-adapt'] },
          extension: [
            {
              url: 'http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaire-questionnaireAdaptive',
              valueUrl: flint_fhir_url
            }
          ],
          url: 'http://flint.health/fhir/Questionnaire/dtr-adapt-v220',
          version: '2.2.0',
          derivedFrom: ['http://flint.health/fhir/Questionnaire/dtr-adapt-v220|2.2.0'],
          status: 'active',
          subjectType: ['Patient'],
          item: [
            { linkId: 'q1', text: 'Primary Oxygen Saturation Reading', type: 'string', required: true },
            { linkId: 'q2', text: 'Confirmed Hypoxemia Diagnosis Details', type: 'string', required: true }
          ]
        }
      ],
      questionnaire: '#adapt-q-1',
      status: 'in-progress',
      subject: { reference: 'Patient/pat-alice-001' },
      authored: '2026-01-15T10:10:00Z',
      item: [
        {
          linkId: 'q1',
          text: 'Primary Oxygen Saturation Reading',
          answer: [{ valueString: '86% on room air' }]
        },
        {
          linkId: 'q2',
          text: 'Confirmed Hypoxemia Diagnosis Details',
          answer: [{ valueString: 'Chronic obstructive pulmonary disease with acute hypoxemia' }]
        }
      ]
    }

    bad_source_req = {
      resourceType: 'Parameters',
      parameter: [
        { name: 'coverage', resource: crd_coverage('cov-cancelled', 'pat-alice-001', status: 'cancelled') },
        { name: 'questionnaire', valueCanonical: 'http://flint.health/fhir/Questionnaire/dtr-home-oxygen|2.2.0' }
      ]
    }

    invalid_qr = {
      resourceType: 'QuestionnaireResponse',
      id: 'dtr-qr-invalid-v220',
      questionnaire: '#adapt-q-1',
      status: 'entered-in-error',
      subject: { reference: 'Patient/pat-alice-001' }
    }

    {
      request_mode: 'manual_mode',
      questionnaire_package_request_parameters: JSON.generate([qp_by_questionnaire, qp_by_context, qp_by_order]),
      questionnaire_response_templates: JSON.generate([qr_template]),
      source_data_error_request: JSON.generate(bad_source_req),
      invalid_questionnaire_response: JSON.generate(invalid_qr),
      questionnaire_canonical: 'http://flint.health/fhir/Questionnaire/dtr-home-oxygen|2.2.0'
    }
  end
end
