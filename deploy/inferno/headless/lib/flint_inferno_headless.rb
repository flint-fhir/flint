# frozen_string_literal: true

# Loaded inside Inferno::Config::Boot::Suites TracePoint so all official HL7
# Inferno test suites (us_core_v610, us_core_v700, us_core_v800, ipa_v100,
# smart_stu2, bulk_data_v200, g10_certification, subscriptions_r5_backport_r4_server)
# register into Inferno::Repositories::TestSuites automatically.
# DaVinciDTRTestKit::MockEHR reads ENV.fetch('FHIR_REFERENCE_SERVER') at gem load time.
ENV['FHIR_REFERENCE_SERVER'] = ENV.fetch('FLINT_FHIR_URL', 'http://127.0.0.1:8080/fhir/r4/demo') if ENV['FHIR_REFERENCE_SERVER'].to_s.strip.empty?

require 'us_core_test_kit'
require 'ipa_test_kit'
require 'smart_app_launch_test_kit'
require 'bulk_data_test_kit'
require 'onc_certification_g10_test_kit'
require 'subscriptions_test_kit'
require 'ips_test_kit'
require 'davinci_plan_net_test_kit'
require 'davinci_us_drug_formulary_test_kit'
require 'carin_for_blue_button_test_kit'
require 'davinci_pdex_test_kit'
require 'davinci_pas_test_kit'
require 'davinci_crd_test_kit'
require 'davinci_dtr_test_kit'
require 'udap_security_test_kit'
