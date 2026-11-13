# frozen_string_literal: true

# Loaded inside Inferno::Config::Boot::Suites TracePoint so all official HL7
# Inferno test suites (us_core_v610, us_core_v700, ipa_v100, smart_stu2,
# bulk_data_v200) register into Inferno::Repositories::TestSuites automatically.
require 'us_core_test_kit'
require 'ipa_test_kit'
require 'smart_app_launch_test_kit'
require 'bulk_data_test_kit'
