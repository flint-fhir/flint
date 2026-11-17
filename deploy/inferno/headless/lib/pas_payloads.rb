# frozen_string_literal: true

require 'json'

# Deterministic HL7 Da Vinci PDex v2.0.0 ($member-match) and Da Vinci PAS v2.2.1
# ($submit, $inquire, Claim Updates, Must-Support, and R5 Backport Subscription)
# conformance payloads for Flint headless CI runs.
module FlintTrack7Payloads
  module_function

  def pdex_member_match_payloads
    consent_for = lambda do |pat_id|
      {
        resourceType: 'Consent',
        id: 'consent-1',
        meta: {
          profile: ['http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-consent']
        },
        status: 'active',
        scope: {
          coding: [{ system: 'http://terminology.hl7.org/CodeSystem/consentscope', code: 'patient-privacy' }]
        },
        category: [
          { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v3-ActCode', code: 'IDSCL' }] }
        ],
        patient: { reference: "Patient/#{pat_id}" },
        performer: [{ reference: "Patient/#{pat_id}" }],
        sourceReference: { reference: 'http://example.org/DocumentReference/pdex-consent-1' },
        policy: [
          { uri: 'http://hl7.org/fhir/us/davinci-hrex/StructureDefinition-hrex-consent.html#regular' }
        ],
        provision: {
          type: 'permit',
          period: { start: '2020-01-01', end: '2099-12-31' },
          actor: [
            {
              role: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/provenance-participant-type', code: 'performer' }] },
              reference: { identifier: { system: 'http://hl7.org/fhir/sid/us-npi', value: '1122334455' }, display: 'Flint Medical Center' }
            },
            {
              role: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v3-ParticipationType', code: 'IRCP' }] },
              reference: { identifier: { system: 'http://hl7.org/fhir/sid/us-npi', value: '9988776655' }, display: 'New Health Plan' }
            }
          ],
          action: [
            { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/consentaction', code: 'disclose' }] }
          ]
        }
      }
    end

    minimal_coverage_to_link = {
      resourceType: 'Coverage',
      id: 'cov-link-1',
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-coverage']
      },
      identifier: [{ system: 'http://newpayer.example.org/coverage', value: 'NEW-COV-001' }],
      status: 'active',
      subscriberId: 'NEW-SUB-001',
      beneficiary: { reference: 'Patient/1' },
      payor: [
        { identifier: { system: 'http://hl7.org/fhir/sid/us-npi', value: '9988776655' }, display: 'New Health Plan' }
      ],
      class: [
        {
          type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/coverage-class', code: 'group' }] },
          value: 'GRP-NEW-01',
          name: 'New Plan Group'
        }
      ]
    }

    one_match = {
      resourceType: 'Parameters',
      id: 'pdex-mm-one',
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-in']
      },
      parameter: [
        {
          name: 'MemberPatient',
          resource: {
            resourceType: 'Patient',
            id: '1',
            meta: { profile: ['http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient'] },
            identifier: [
              {
                type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v2-0203', code: 'MB' }] },
                system: 'http://hospital.example.org/mrn',
                value: 'MRN-1001'
              }
            ],
            name: [{ use: 'official', family: 'Wonderland', given: %w[Alice Marie] }],
            gender: 'female',
            birthDate: '1985-04-12'
          }
        },
        {
          name: 'CoverageToMatch',
          resource: {
            resourceType: 'Coverage',
            id: 'cov-match-1',
            meta: { profile: ['http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-coverage'] },
            identifier: [{ system: 'http://flint-health.example.org/memberid', value: 'MEM-ALICE-001' }],
            status: 'draft',
            subscriberId: 'SUB-ALICE-001',
            beneficiary: { reference: 'Patient/1' },
            payor: [
              { identifier: { system: 'http://hl7.org/fhir/sid/us-npi', value: '1122334455' }, display: 'Flint Medical Center' }
            ]
          }
        },
        {
          name: 'CoverageToLink',
          resource: minimal_coverage_to_link
        },
        {
          name: 'Consent',
          resource: consent_for.call('1')
        }
      ]
    }

    no_match = {
      resourceType: 'Parameters',
      id: 'pdex-mm-none',
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-in']
      },
      parameter: [
        {
          name: 'MemberPatient',
          resource: {
            resourceType: 'Patient',
            id: '1',
            meta: { profile: ['http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient'] },
            identifier: [
              {
                type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v2-0203', code: 'MB' }] },
                system: 'http://oldpayer.example.org/member',
                value: 'NOMATCH-999999'
              }
            ],
            name: [{ use: 'official', family: 'NonexistentPerson', given: ['Nobody'] }],
            gender: 'other',
            birthDate: '1901-01-01'
          }
        },
        {
          name: 'CoverageToMatch',
          resource: {
            resourceType: 'Coverage',
            id: 'cov-nomatch-1',
            meta: { profile: ['http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-coverage'] },
            identifier: [{ system: 'http://oldpayer.example.org/coverage', value: 'COV-NOMATCH-999' }],
            status: 'draft',
            subscriberId: 'SUB-NOMATCH-999999',
            beneficiary: { reference: 'Patient/1' },
            payor: [
              { identifier: { system: 'http://hl7.org/fhir/sid/us-npi', value: '1122334455' }, display: 'Old Health Plan' }
            ]
          }
        },
        {
          name: 'Consent',
          resource: consent_for.call('1')
        }
      ]
    }

    multi_match = {
      resourceType: 'Parameters',
      id: 'pdex-mm-multi',
      meta: {
        profile: ['http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-parameters-member-match-in']
      },
      parameter: [
        {
          name: 'MemberPatient',
          resource: {
            resourceType: 'Patient',
            id: '1',
            meta: { profile: ['http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient'] },
            identifier: [
              {
                type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v2-0203', code: 'MB' }] },
                system: 'http://flint.health/pdex/dup',
                value: 'SUB-PDEX-DUP-999'
              }
            ],
            name: [{ use: 'official', family: 'Dupont', given: ['Jean'] }],
            gender: 'male',
            birthDate: '1991-03-11'
          }
        },
        {
          name: 'CoverageToMatch',
          resource: {
            resourceType: 'Coverage',
            id: 'cov-multi-1',
            meta: { profile: ['http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/hrex-coverage'] },
            identifier: [{ system: 'http://flint.health/pdex/dup', value: 'SUB-PDEX-DUP-999' }],
            status: 'draft',
            subscriberId: 'SUB-PDEX-DUP-999',
            beneficiary: { reference: 'Patient/1' },
            payor: [
              { identifier: { system: 'http://hl7.org/fhir/sid/us-npi', value: '1122334455' }, display: 'Flint Medical Center' }
            ]
          }
        },
        {
          name: 'Consent',
          resource: consent_for.call('1')
        }
      ]
    }

    {
      member_match_request: JSON.generate(one_match),
      no_member_match_request: JSON.generate(no_match),
      multiple_member_match_request: JSON.generate(multi_match)
    }
  end

  def pas_v221_inputs(notify_endpoint:, notify_token:)
    insurer_id   = '11111111-1111-4111-8111-000000000001'
    requestor_id = '11111111-1111-4111-8111-000000000002'
    pat_id       = '11111111-1111-4111-8111-000000000003'
    cov_id       = '11111111-1111-4111-8111-000000000004'
    prac_id      = '11111111-1111-4111-8111-000000000005'
    loc_id       = '11111111-1111-4111-8111-000000000006'
    pracrole_id  = '11111111-1111-4111-8111-000000000007'
    sr_id        = '11111111-1111-4111-8111-000000000008'
    enc_id       = '11111111-1111-4111-8111-000000000009'
    docref_id    = '11111111-1111-4111-8111-000000000010'

    urn = ->(uuid) { "urn:uuid:#{uuid}" }

    insurer_entry = {
      fullUrl: urn.call(insurer_id),
      resource: {
        resourceType: 'Organization',
        id: insurer_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-insurer'] },
        identifier: [
          { system: 'http://hl7.org/fhir/sid/us-npi', value: '8383066098' },
          { system: 'http://terminology.hl7.org/NamingSystem/USEIN', value: '7239994834' },
          {
            type: { coding: [{ system: 'http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType', code: 'payerid' }] },
            system: 'http://flint.health/pas/payerid',
            value: 'PI-INS-001'
          }
        ],
        active: true,
        type: [{ coding: [{ system: 'https://codesystem.x12.org/005010/98', code: 'PR' }] }],
        name: 'Flint PAS Payer Organization',
        telecom: [{ system: 'phone', value: '800-555-0188' }],
        address: [{ line: ['100 Payer Way'], city: 'Boston', state: 'MA', postalCode: '02110', country: 'US' }]
      }
    }

    requestor_entry = {
      fullUrl: urn.call(requestor_id),
      resource: {
        resourceType: 'Organization',
        id: requestor_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-requestor'] },
        identifier: [
          { system: 'http://hl7.org/fhir/sid/us-npi', value: '8189991234' },
          { system: 'http://terminology.hl7.org/NamingSystem/USEIN', value: '8189948234' },
          {
            type: { coding: [{ system: 'http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType', code: 'payerid' }] },
            system: 'http://flint.health/pas/payerid',
            value: 'PI-REQ-001'
          }
        ],
        active: true,
        type: [{ coding: [{ system: 'https://codesystem.x12.org/005010/98', code: 'X3' }] }],
        name: 'Flint PAS Requestor Medical Group',
        telecom: [{ system: 'phone', value: '617-555-0177' }],
        address: [{ line: ['200 Provider Ave'], city: 'Boston', state: 'MA', postalCode: '02115', country: 'US' }],
        contact: [
          {
            name: { family: 'Watson', given: ['Susan'] },
            telecom: [{ system: 'phone', value: '617-555-0178' }]
          }
        ]
      }
    }

    patient_entry = {
      fullUrl: urn.call(pat_id),
      resource: {
        resourceType: 'Patient',
        id: pat_id,
        meta: {
          profile: [
            'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-beneficiary',
            'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-subscriber'
          ]
        },
        extension: [
          {
            url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-militaryStatus',
            valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/584', code: 'RU' }] }
          }
        ],
        identifier: [{ system: 'http://flint.health/pas/member', value: 'PAS-MEM-001' }],
        name: [{ family: 'Smith', given: ['Joe'], prefix: ['Mr.'], suffix: ['Jr.'] }],
        telecom: [{ system: 'phone', value: '555-555-0101', use: 'home' }],
        gender: 'male',
        birthDate: '1987-02-20',
        multipleBirthInteger: 1,
        address: [
          {
            line: ['49 Meadow St'],
            city: 'Boston',
            district: 'Suffolk',
            state: 'MA',
            postalCode: '02115',
            country: 'US'
          }
        ],
        communication: [
          {
            language: { coding: [{ system: 'urn:ietf:bcp:47', code: 'en-US', display: 'English (United States)' }] },
            preferred: true
          }
        ]
      }
    }

    coverage_entry = {
      fullUrl: urn.call(cov_id),
      resource: {
        resourceType: 'Coverage',
        id: cov_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-coverage'] },
        identifier: [
          {
            type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v2-0203', code: 'MB' }] },
            system: 'http://flint.health/pas/memberid',
            value: 'MB-PAS-001'
          }
        ],
        status: 'active',
        type: { coding: [{ system: 'https://nahdo.org/sopt', code: '1', display: 'MEDICARE' }] },
        subscriber: { reference: urn.call(pat_id) },
        subscriberId: 'PAS-MEM-001',
        beneficiary: { reference: urn.call(pat_id) },
        relationship: {
          coding: [
            { system: 'http://terminology.hl7.org/CodeSystem/subscriber-relationship', code: 'self' },
            { system: 'https://codesystem.x12.org/005010/1069', code: '18' }
          ]
        },
        period: { start: '2026-01-01', end: '2026-12-31' },
        payor: [{ reference: urn.call(insurer_id) }],
        class: [
          {
            type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/coverage-class', code: 'group' }] },
            value: 'GRP-100',
            name: 'Flint PAS Group 100'
          },
          {
            type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/coverage-class', code: 'plan' }] },
            value: 'PLN-200',
            name: 'Flint PAS Gold Plan'
          }
        ]
      }
    }

    practitioner_entry = {
      fullUrl: urn.call(prac_id),
      resource: {
        resourceType: 'Practitioner',
        id: prac_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-practitioner'] },
        identifier: [{ system: 'http://hl7.org/fhir/sid/us-npi', value: '1234567893' }],
        name: [{ family: 'Watson', given: ['Susan'] }],
        telecom: [{ system: 'phone', value: '617-555-0190' }],
        address: [{ line: ['200 Provider Ave'], city: 'Boston', state: 'MA', postalCode: '02115', country: 'US' }]
      }
    }

    location_entry = {
      fullUrl: urn.call(loc_id),
      resource: {
        resourceType: 'Location',
        id: loc_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-location'] },
        status: 'active',
        name: 'Flint PAS Referral Clinic',
        address: { line: ['200 Provider Ave'], city: 'Boston', state: 'MA', postalCode: '02115', country: 'US' }
      }
    }

    pracrole_entry = {
      fullUrl: urn.call(pracrole_id),
      resource: {
        resourceType: 'PractitionerRole',
        id: pracrole_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-practitionerrole'] },
        practitioner: { reference: urn.call(prac_id) },
        organization: { reference: urn.call(requestor_id) },
        location: [{ reference: urn.call(loc_id) }],
        code: [{ coding: [{ system: 'http://nucc.org/provider-taxonomy', code: '207RC0000X' }] }],
        specialty: [{ coding: [{ system: 'http://nucc.org/provider-taxonomy', code: '207RC0000X' }] }],
        telecom: [{ system: 'phone', value: '617-555-0191' }],
        endpoint: [{ display: 'Direct Messaging Endpoint' }]
      }
    }

    servicerequest_entry = {
      fullUrl: urn.call(sr_id),
      resource: {
        resourceType: 'ServiceRequest',
        id: sr_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-servicerequest'] },
        extension: [
          {
            url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-productOrServiceCodeEnd',
            valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] }
          },
          {
            url: 'http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information',
            extension: [
              { url: 'coverage', valueReference: { reference: urn.call(cov_id) } },
              { url: 'covered', valueCode: 'conditional' },
              { url: 'pa-needed', valueCode: 'auth-needed' },
              { url: 'date', valueDate: '2026-02-01' },
              { url: 'coverage-assertion-id', valueString: 'ASSERT-PAS-001' }
            ]
          }
        ],
        status: 'active',
        intent: 'order',
        code: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
        quantityQuantity: { value: 1, unit: 'visit', system: 'https://codesystem.x12.org/005010/673', code: 'VS' },
        subject: { reference: urn.call(pat_id) },
        occurrenceTiming: { repeat: { frequency: 1, period: 1, periodUnit: 'd' } }
      }
    }

    encounter_entry = {
      fullUrl: urn.call(enc_id),
      resource: {
        resourceType: 'Encounter',
        id: enc_id,
        meta: {
          lastUpdated: '2026-02-01T10:00:00Z',
          profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-encounter']
        },
        extension: [
          {
            url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-patientStatus',
            valueCodeableConcept: { coding: [{ system: 'https://www.nubc.org/CodeSystem/PatDischargeStatus', code: '01' }] }
          },
          {
            url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-nursingHomeResidentialStatus',
            valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1345', code: '2' }] }
          }
        ],
        identifier: [{ system: 'http://flint.health/pas/encounter', value: 'ENC-PAS-001' }],
        status: 'planned',
        class: { system: 'http://terminology.hl7.org/CodeSystem/v3-ActCode', code: 'AMB' },
        type: [{ coding: [{ system: 'http://snomed.info/sct', code: '11429006', display: 'Consultation' }] }],
        subject: { reference: urn.call(pat_id) },
        participant: [
          {
            type: [{ coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v3-ParticipationType', code: 'ATND' }] }],
            period: { start: '2026-02-10', end: '2026-02-11' },
            individual: { reference: urn.call(prac_id) }
          }
        ],
        period: { start: '2026-02-10', end: '2026-02-11' },
        reasonCode: [{ coding: [{ system: 'http://hl7.org/fhir/sid/icd-10-cm', code: 'G89.4' }] }],
        reasonReference: [{ display: 'Chronic pain evaluation' }],
        hospitalization: {
          admitSource: { coding: [{ system: 'https://www.nubc.org/CodeSystem/PointOfOrigin', code: '1' }] },
          dischargeDisposition: { coding: [{ system: 'https://www.nubc.org/CodeSystem/PatDischargeStatus', code: '01' }] }
        },
        location: [{ location: { reference: urn.call(loc_id) } }],
        serviceProvider: { reference: urn.call(requestor_id) }
      }
    }

    docref_entry = {
      fullUrl: urn.call(docref_id),
      resource: {
        resourceType: 'DocumentReference',
        id: docref_id,
        meta: { profile: ['http://hl7.org/fhir/us/core/StructureDefinition/us-core-documentreference'] },
        identifier: [{ system: 'urn:ietf:rfc:3986', value: 'urn:oid:2.16.840.1.113883.19.5.99999.1' }],
        status: 'current',
        type: { coding: [{ system: 'http://loinc.org', code: '34133-9', display: 'Summary of episode note' }] },
        category: [
          { coding: [{ system: 'http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category', code: 'clinical-note' }] }
        ],
        subject: { reference: urn.call(pat_id) },
        date: '2026-02-01T09:00:00Z',
        author: [{ reference: urn.call(prac_id) }],
        content: [
          { attachment: { contentType: 'text/plain', data: 'U3VwcG9ydGluZyBjbGluaWNhbCBub3Rl' } }
        ]
      }
    }

    make_basic_item = lambda do |seq, cert_code: 'I', cancelled: false, info_changed: nil|
      exts = [
        {
          url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber',
          valueIdentifier: { system: 'http://flint.health/pas/item-trace', value: "TRACE-#{seq}" }
        },
        {
          url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceItemRequestType',
          valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1525', code: 'SC', display: 'Specialty Care Review' }] }
        },
        {
          url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType',
          valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1322', code: cert_code }] }
        },
        {
          url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-requestedService',
          valueReference: { reference: urn.call(sr_id) }
        }
      ]
      if info_changed
        exts << {
          url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-infoChanged',
          valueCode: info_changed
        }
      end
      item = {
        extension: exts,
        sequence: seq,
        careTeamSequence: [1],
        category: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
        productOrService: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
        servicedDate: '2026-02-10',
        locationCodeableConcept: { coding: [{ system: 'https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set', code: '11' }] },
        quantity: { value: 1, unit: 'unit', system: 'https://codesystem.x12.org/005010/673', code: 'UN' }
      }
      if cancelled
        item[:modifierExtension] = [
          { url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/modifierextension-infoCancelled', valueBoolean: true }
        ]
      end
      item
    end

    make_scenario_bundle = lambda do |bundle_uuid, claim_uuid, trace_val, items:, prior_entry: nil, claim_cert_code: 'I'|
      is_update = !prior_entry.nil?
      claim_profile = is_update ? 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update' : 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim'
      claim_exts = [
        {
          url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-levelOfServiceCode',
          valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1338', code: 'U', display: 'Urgent' }] }
        }
      ]
      if is_update
        claim_exts << {
          url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType',
          valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1322', code: claim_cert_code }] }
        }
      end
      claim_res = {
        resourceType: 'Claim',
        id: claim_uuid,
        meta: { profile: [claim_profile] },
        extension: claim_exts,
        identifier: [{ system: 'http://flint.health/pas/trace', value: trace_val }],
        status: is_update && claim_cert_code == '3' ? 'cancelled' : 'active',
        type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claim-type', code: 'professional' }] },
        use: 'preauthorization',
        patient: { reference: urn.call(pat_id) },
        created: '2026-02-01T10:00:00Z',
        insurer: { reference: urn.call(insurer_id) },
        provider: { reference: urn.call(requestor_id) },
        priority: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/processpriority', code: 'normal' }] },
        careTeam: [
          {
            extension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-careTeamClaimScope', valueBoolean: true }],
            sequence: 1,
            provider: { reference: urn.call(pracrole_id) },
            role: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claimcareteamrole', code: 'primary' }] }
          }
        ],
        insurance: [{ sequence: 1, focal: true, coverage: { reference: urn.call(cov_id) } }]
      }
      if is_update
        claim_res[:related] = [
          {
            claim: { reference: prior_entry[:fullUrl] },
            relationship: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship', code: 'prior' }] }
          }
        ]
      end
      claim_res[:item] = items unless items.empty?

      claim_entry = { fullUrl: urn.call(claim_uuid), resource: claim_res }
      entries = [
        claim_entry,
        patient_entry,
        insurer_entry,
        requestor_entry,
        coverage_entry,
        pracrole_entry,
        practitioner_entry,
        location_entry
      ]
      entries << servicerequest_entry unless items.empty?
      entries << prior_entry if prior_entry

      bundle = {
        resourceType: 'Bundle',
        id: bundle_uuid,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle'] },
        identifier: { system: 'http://flint.health/pas/bundle', value: trace_val },
        type: 'collection',
        timestamp: '2026-02-01T10:00:00Z',
        entry: entries
      }
      [bundle, claim_entry]
    end

    approval_bundle, = make_scenario_bundle.call(
      '22222222-2222-4222-8222-000000000001',
      '22222222-2222-4222-8222-000000000011',
      'PA-APPROVAL-001',
      items: [make_basic_item.call(1)]
    )
    denial_bundle, = make_scenario_bundle.call(
      '22222222-2222-4222-8222-000000000002',
      '22222222-2222-4222-8222-000000000012',
      'PA-DENIAL-001',
      items: [make_basic_item.call(1)]
    )
    pended_bundle, = make_scenario_bundle.call(
      '22222222-2222-4222-8222-000000000003',
      '22222222-2222-4222-8222-000000000013',
      'PA-PENDED-001',
      items: [make_basic_item.call(1)]
    )

    # 4-step Claim Updates sequence (note: grandchild omits grandparent `related` per spec-65/66)
    upd1_bundle, upd1_claim_entry = make_scenario_bundle.call(
      '33333333-3333-4333-8333-000000000001',
      '33333333-3333-4333-8333-000000000011',
      'PA-UPDATE-INIT-001',
      items: [make_basic_item.call(1)]
    )
    upd2_bundle, upd2_claim_entry = make_scenario_bundle.call(
      '33333333-3333-4333-8333-000000000002',
      '33333333-3333-4333-8333-000000000012',
      'PA-UPDATE-ADD-002',
      items: [make_basic_item.call(1), make_basic_item.call(2, info_changed: 'added')],
      prior_entry: upd1_claim_entry,
      claim_cert_code: 'R'
    )
    upd2_parent_no_grandparent = Marshal.load(Marshal.dump(upd2_claim_entry))
    upd2_parent_no_grandparent[:resource].delete(:related)
    upd3_bundle, upd3_claim_entry = make_scenario_bundle.call(
      '33333333-3333-4333-8333-000000000003',
      '33333333-3333-4333-8333-000000000013',
      'PA-UPDATE-MODCANCEL-003',
      items: [
        make_basic_item.call(1, cert_code: 'R', info_changed: 'changed'),
        make_basic_item.call(2, cert_code: '3', cancelled: true)
      ],
      prior_entry: upd2_parent_no_grandparent,
      claim_cert_code: 'R'
    )
    upd3_parent_no_grandparent = Marshal.load(Marshal.dump(upd3_claim_entry))
    upd3_parent_no_grandparent[:resource].delete(:related)
    upd3_parent_no_grandparent[:resource].delete(:item)
    upd4_bundle, = make_scenario_bundle.call(
      '33333333-3333-4333-8333-000000000004',
      '33333333-3333-4333-8333-000000000014',
      'PA-UPDATE-CANCELALL-004',
      items: [],
      prior_entry: upd3_parent_no_grandparent,
      claim_cert_code: '3'
    )

    # Rich Must-Support $submit payload (conforms to profile-claim-update with ALL MS slices/extensions/elements)
    ms_prior_claim_id = '44444444-4444-4444-8444-000000000010'
    ms_prior_claim_entry = {
      fullUrl: urn.call(ms_prior_claim_id),
      resource: {
        resourceType: 'Claim',
        id: ms_prior_claim_id,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim'] },
        identifier: [{ system: 'http://flint.health/pas/trace', value: 'PA-PRIOR-MS-000' }],
        status: 'active',
        type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claim-type', code: 'professional' }] },
        use: 'preauthorization',
        patient: { reference: urn.call(pat_id) },
        created: '2026-01-25T10:00:00Z',
        insurer: { reference: urn.call(insurer_id) },
        provider: { reference: urn.call(requestor_id) },
        priority: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/processpriority', code: 'normal' }] },
        insurance: [{ sequence: 1, focal: true, coverage: { reference: urn.call(cov_id) } }],
        item: [make_basic_item.call(1)]
      }
    }

    ms_submit_claim_id = '44444444-4444-4444-8444-000000000011'
    ms_submit_bundle = {
      resourceType: 'Bundle',
      id: '44444444-4444-4444-8444-000000000001',
      meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle'] },
      identifier: { system: 'http://flint.health/pas/bundle', value: 'PA-MUST-SUPPORT-SUBMIT-001' },
      type: 'collection',
      timestamp: '2026-02-01T10:00:00Z',
      entry: [
        {
          fullUrl: urn.call(ms_submit_claim_id),
          resource: {
            resourceType: 'Claim',
            id: ms_submit_claim_id,
            meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update'] },
            extension: [
              {
                url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers',
                extension: [
                  { url: 'applicationSenderCode', valueString: 'FLINT-PROV' },
                  { url: 'applicationReceiverCode', valueString: 'FLINT-PAYER' }
                ]
              },
              {
                url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-levelOfServiceCode',
                valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1338', code: 'U', display: 'Urgent' }] }
              },
              {
                url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-conditionCode',
                valueCodeableConcept: { coding: [{ system: 'https://www.nubc.org/CodeSystem/ConditionCodes', code: '01' }] }
              },
              {
                url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-homeHealthCareInformation',
                extension: [
                  { url: 'prognosis', valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/923', code: '1' }] } },
                  { url: 'date', valueDate: '2026-02-01' }
                ]
              },
              {
                url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber',
                valueString: 'AUTH-CLAIM-MS-001'
              },
              {
                url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber',
                valueString: 'ADMIN-CLAIM-MS-001'
              },
              {
                url: 'http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter',
                valueReference: { reference: urn.call(enc_id) }
              },
              {
                url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType',
                valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1322', code: 'R', display: 'Revision' }] }
              }
            ],
            identifier: [
              {
                extension: [
                  { url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-identifierSubDepartment', valueString: 'SUBDEPT-MS' },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-identifierJurisdiction',
                    valueCodeableConcept: { coding: [{ system: 'https://www.usps.com/', code: 'MA' }] }
                  }
                ],
                system: 'http://flint.health/pas/trace',
                value: 'PA-MUST-SUPPORT-SUBMIT-001'
              }
            ],
            status: 'active',
            type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claim-type', code: 'professional' }] },
            use: 'preauthorization',
            patient: { reference: urn.call(pat_id) },
            created: '2026-02-01T10:00:00Z',
            insurer: { reference: urn.call(insurer_id) },
            provider: { reference: urn.call(requestor_id) },
            priority: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/processpriority', code: 'normal' }] },
            related: [
              {
                claim: { reference: urn.call(ms_prior_claim_id) },
                relationship: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship', code: 'prior' }] }
              }
            ],
            careTeam: [
              {
                extension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-careTeamClaimScope', valueBoolean: true }],
                sequence: 1,
                provider: { reference: urn.call(pracrole_id) },
                role: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claimcareteamrole', code: 'primary' }] },
                qualification: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/ex-providerqualification', code: '604210' }] }
              },
              {
                extension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-careTeamClaimScope', valueBoolean: false }],
                sequence: 2,
                provider: { reference: urn.call(prac_id) },
                role: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claimcareteamrole', code: 'assist' }] },
                qualification: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/ex-providerqualification', code: '604210' }] }
              }
            ],
            supportingInfo: [
              {
                extension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-infoChanged', valueCode: 'changed' }],
                sequence: 1,
                category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'patientEvent' }] },
                timingPeriod: { start: '2026-02-01', end: '2026-02-28' }
              },
              {
                sequence: 2,
                category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'admissionDates' }] },
                timingPeriod: { start: '2026-02-10', end: '2026-02-12' }
              },
              {
                sequence: 3,
                category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'dischargeDates' }] },
                timingDate: '2026-02-12'
              },
              {
                extension: [
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-documentInformation',
                    extension: [
                      { url: 'reportTypeCode', valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/755', code: '15' }] } },
                      { url: 'transmissionMethod', valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/756', code: 'EL' }] } },
                      { url: 'controlNumber', valueString: 'DOC-CTRL-001' },
                      { url: 'description', valueString: 'Clinical summary attachment' }
                    ]
                  }
                ],
                sequence: 4,
                category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'additionalInformation' }] },
                valueReference: { reference: urn.call(docref_id) }
              },
              {
                modifierExtension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/modifierextension-infoCancelled', valueBoolean: true }],
                sequence: 5,
                category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'freeFormMessage' }] },
                valueString: 'Supplemental clinical review note'
              }
            ],
            diagnosis: [
              {
                extension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-diagnosisRecordedDate', valueDate: '2026-01-15' }],
                sequence: 1,
                diagnosisCodeableConcept: { coding: [{ system: 'http://hl7.org/fhir/sid/icd-10-cm', code: 'G89.4' }] },
                type: [{ coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'patientreasonforvisit' }] }]
              }
            ],
            insurance: [{ sequence: 1, focal: true, coverage: { reference: urn.call(cov_id) } }],
            accident: {
              date: '2026-01-10',
              type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v3-ActCode', code: 'MVA' }] },
              locationAddress: { line: ['100 Main St'], city: 'Boston', state: 'MA', postalCode: '02110', country: 'US' }
            },
            item: [
              {
                extension: [
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber',
                    valueIdentifier: { system: 'http://flint.health/pas/item-trace', value: 'TRACE-MS-1' }
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber',
                    valueString: 'AUTH-MS-OLD-001'
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber',
                    valueString: 'ADMIN-MS-001'
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceItemRequestType',
                    valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1525', code: 'SC', display: 'Specialty Care Review' }] }
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType',
                    valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1322', code: 'R', display: 'Revision' }] }
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-productOrServiceCodeEnd',
                    valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3' }] }
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-epsdtIndicator',
                    valueBoolean: false
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-nursingHomeResidentialStatus',
                    valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1345', code: '2' }] }
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-nursingHomeLevelOfCare',
                    valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1337', code: '2' }] }
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-revenueUnitRateLimit',
                    valueDecimal: 100.0
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-requestedService',
                    valueReference: { reference: urn.call(sr_id) }
                  },
                  {
                    url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-infoChanged',
                    valueCode: 'changed'
                  }
                ],
                sequence: 1,
                careTeamSequence: [2],
                diagnosisSequence: [1],
                informationSequence: [1, 4],
                revenue: { coding: [{ system: 'https://www.nubc.org/CodeSystem/RevenueCodes', code: '0300' }] },
                category: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                productOrService: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                modifier: [{ coding: [{ system: 'http://www.ama-assn.org/go/cpt', code: '25' }] }],
                servicedDate: '2026-02-10',
                locationCodeableConcept: { coding: [{ system: 'https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set', code: '11' }] },
                quantity: { value: 1, unit: 'unit', system: 'https://codesystem.x12.org/005010/673', code: 'UN' },
                unitPrice: { value: 250.0, currency: 'USD' }
              },
              {
                modifierExtension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/modifierextension-infoCancelled', valueBoolean: true }],
                sequence: 2,
                category: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                productOrService: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                servicedPeriod: { start: '2026-02-11', end: '2026-02-12' },
                quantity: { value: 1, unit: 'unit', system: 'https://codesystem.x12.org/005010/673', code: 'UN' }
              }
            ]
          }
        },
        ms_prior_claim_entry,
        patient_entry,
        insurer_entry,
        requestor_entry,
        coverage_entry,
        pracrole_entry,
        practitioner_entry,
        location_entry,
        servicerequest_entry,
        encounter_entry,
        docref_entry
      ]
    }

    # Rich Must-Support $inquire payloads (2-Bundle array covering both ClaimResponse.request.reference and DAR + replaced PA auth number!)
    make_inquire_bundle = lambda do |bundle_uuid, claim_uuid, trace_val|
      {
        resourceType: 'Bundle',
        id: bundle_uuid,
        meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-inquiry-request-bundle'] },
        identifier: { system: 'http://flint.health/pas/inquire-bundle', value: trace_val },
        type: 'collection',
        timestamp: '2026-02-02T10:00:00Z',
        entry: [
          {
            fullUrl: urn.call(claim_uuid),
            resource: {
              resourceType: 'Claim',
              id: claim_uuid,
              meta: { profile: ['http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-inquiry'] },
              extension: [
                {
                  url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-TransmissionIdentifiers',
                  extension: [
                    { url: 'applicationSenderCode', valueString: 'FLINT-PROV' },
                    { url: 'applicationReceiverCode', valueString: 'FLINT-PAYER' }
                  ]
                },
                {
                  url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-levelOfServiceCode',
                  valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1338', code: 'U', display: 'Urgent' }] }
                },
                {
                  url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-conditionCode',
                  valueCodeableConcept: { coding: [{ system: 'https://www.nubc.org/CodeSystem/ConditionCodes', code: '01' }] }
                },
                {
                  url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-homeHealthCareInformation',
                  extension: [
                    { url: 'prognosis', valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/923', code: '1' }] } },
                    { url: 'date', valueDate: '2026-02-01' }
                  ]
                },
                {
                  url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber',
                  valueString: 'AUTH-INQ-CLAIM-001'
                },
                {
                  url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber',
                  valueString: 'ADMIN-INQ-CLAIM-001'
                },
                {
                  url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType',
                  valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1322', code: 'I', display: 'Initial' }] }
                }
              ],
              identifier: [
                {
                  extension: [
                    { url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-identifierSubDepartment', valueString: 'SUBDEPT-INQ' },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-identifierJurisdiction',
                      valueCodeableConcept: { coding: [{ system: 'https://www.usps.com/', code: 'MA' }] }
                    }
                  ],
                  system: 'http://flint.health/pas/trace',
                  value: trace_val
                }
              ],
              status: 'active',
              type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claim-type', code: 'professional' }] },
              use: 'preauthorization',
              patient: { reference: urn.call(pat_id) },
              billablePeriod: { start: '2026-02-01', end: '2026-02-28' },
              created: '2026-02-02T10:00:00Z',
              insurer: { reference: urn.call(insurer_id) },
              provider: { reference: urn.call(requestor_id) },
              priority: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/processpriority', code: 'normal' }] },
              careTeam: [
                {
                  extension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-careTeamClaimScope', valueBoolean: true }],
                  sequence: 1,
                  provider: { reference: urn.call(pracrole_id) },
                  role: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claimcareteamrole', code: 'primary' }] },
                  qualification: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/ex-providerqualification', code: '604210' }] }
                },
                {
                  extension: [{ url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-careTeamClaimScope', valueBoolean: false }],
                  sequence: 2,
                  provider: { reference: urn.call(prac_id) },
                  role: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/claimcareteamrole', code: 'assist' }] },
                  qualification: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/ex-providerqualification', code: '604210' }] }
                }
              ],
              supportingInfo: [
                {
                  sequence: 1,
                  category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'patientEvent' }] },
                  timingPeriod: { start: '2026-02-01', end: '2026-02-28' }
                },
                {
                  sequence: 2,
                  category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'admissionDates' }] },
                  timingPeriod: { start: '2026-02-10', end: '2026-02-12' }
                },
                {
                  sequence: 3,
                  category: { coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'dischargeDates' }] },
                  timingDate: '2026-02-12'
                }
              ],
              diagnosis: [
                {
                  sequence: 1,
                  diagnosisCodeableConcept: { coding: [{ system: 'http://hl7.org/fhir/sid/icd-10-cm', code: 'G89.4' }] },
                  type: [{ coding: [{ system: 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes', code: 'patientreasonforvisit' }] }]
                }
              ],
              insurance: [{ sequence: 1, focal: true, coverage: { reference: urn.call(cov_id) } }],
              accident: {
                date: '2026-01-10',
                type: { coding: [{ system: 'http://terminology.hl7.org/CodeSystem/v3-ActCode', code: 'MVA' }] },
                locationAddress: { line: ['100 Main St'], city: 'Boston', state: 'MA', postalCode: '02110', country: 'US' }
              },
              item: [
                {
                  extension: [
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber',
                      valueIdentifier: { system: 'http://flint.health/pas/item-trace', value: 'TRACE-INQ-1' }
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber',
                      valueString: 'AUTH-OLD-001'
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-administrationReferenceNumber',
                      valueString: 'ADMIN-INQ-001'
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-serviceItemRequestType',
                      valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1525', code: 'SC', display: 'Specialty Care Review' }] }
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-certificationType',
                      valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1322', code: 'I', display: 'Initial' }] }
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-productOrServiceCodeEnd',
                      valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3' }] }
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemCertificationIssueDate',
                      valueDate: '2026-02-01'
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemCertificationExpirationDate',
                      valueDate: '2026-05-01'
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemCertificationEffectiveDate',
                      valueDate: '2026-02-01'
                    },
                    {
                      url: 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode',
                      valueCodeableConcept: { coding: [{ system: 'https://codesystem.x12.org/005010/306', code: 'A1', display: 'Certified in total' }] }
                    }
                  ],
                  sequence: 1,
                  careTeamSequence: [2],
                  diagnosisSequence: [1],
                  informationSequence: [1],
                  revenue: { coding: [{ system: 'https://www.nubc.org/CodeSystem/RevenueCodes', code: '0300' }] },
                  category: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                  productOrService: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                  modifier: [{ coding: [{ system: 'http://www.ama-assn.org/go/cpt', code: '25' }] }],
                  servicedDate: '2026-02-10',
                  locationCodeableConcept: { coding: [{ system: 'https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set', code: '11' }] },
                  quantity: { value: 1, unit: 'unit', system: 'https://codesystem.x12.org/005010/673', code: 'UN' }
                },
                {
                  sequence: 2,
                  category: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                  productOrService: { coding: [{ system: 'https://codesystem.x12.org/005010/1365', code: '3', display: 'Consultation' }] },
                  servicedPeriod: { start: '2026-02-11', end: '2026-02-12' },
                  quantity: { value: 1, unit: 'unit', system: 'https://codesystem.x12.org/005010/673', code: 'UN' }
                }
              ]
            }
          },
          patient_entry,
          insurer_entry,
          requestor_entry,
          coverage_entry,
          pracrole_entry,
          practitioner_entry,
          location_entry
        ]
      }
    end

    inq_ref_bundle = make_inquire_bundle.call(
      '55555555-5555-4555-8555-000000000001',
      '55555555-5555-4555-8555-000000000011',
      'PA-MS-INQUIRE-001'
    )
    inq_dar_bundle = make_inquire_bundle.call(
      '55555555-5555-4555-8555-000000000002',
      '55555555-5555-4555-8555-000000000012',
      'PA-MS-INQUIRE-DAR-002'
    )

    pas_subscription = {
      resourceType: 'Subscription',
      meta: {
        profile: [
          'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-subscription',
          'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription'
        ]
      },
      status: 'requested',
      end: '2099-12-31T12:00:00Z',
      reason: 'PAS Pended Prior Authorization Finalization Subscription',
      criteria: 'http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic',
      _criteria: {
        extension: [
          {
            url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria',
            valueString: 'org-identifier=8189991234'
          }
        ]
      },
      channel: {
        extension: [
          {
            url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-timeout',
            valueUnsignedInt: 60
          },
          {
            url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-max-count',
            valuePositiveInt: 20
          }
        ],
        type: 'rest-hook',
        header: ["Authorization: Bearer #{notify_token}"],
        endpoint: notify_endpoint,
        payload: 'application/fhir+json',
        _payload: {
          extension: [
            {
              url: 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content',
              valueCode: 'full-resource'
            }
          ]
        }
      }
    }

    {
      subscription_resource: JSON.generate(pas_subscription),
      approval_pa_submit_request_payload: JSON.generate(approval_bundle),
      denial_pa_submit_request_payload: JSON.generate(denial_bundle),
      pended_pa_submit_request_payload: JSON.generate(pended_bundle),
      claim_update_initial_submit_payload: JSON.generate(upd1_bundle),
      claim_update_add_item_submit_payload: JSON.generate(upd2_bundle),
      claim_update_modify_cancel_submit_payload: JSON.generate(upd3_bundle),
      claim_update_cancel_all_submit_payload: JSON.generate(upd4_bundle),
      must_support_pa_submit_request_payload: JSON.generate(ms_submit_bundle),
      must_support_pa_inquire_request_payload: JSON.generate([inq_ref_bundle, inq_dar_bundle])
    }
  end
end
