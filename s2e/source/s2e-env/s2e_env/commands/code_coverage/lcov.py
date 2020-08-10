"""
Copyright (c) 2017 Dependable Systems Laboratory, EPFL
Copyright (c) 2017-2018 Cyberhaven

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
"""


import logging
import os
import sys

# pylint: disable=no-name-in-module
from sh import genhtml, ErrorReturnCode

from s2e_env.command import ProjectCommand, CommandError
from s2e_env.symbols import SymbolManager
from . import get_tb_files, aggregate_tb_files

logger = logging.getLogger('lcov')


def _merge_coverage(coverage, module_name, module_coverage):
    if module_name not in coverage:
        coverage[module_name] = module_coverage

    cov = coverage[module_name]
    for addr, cnt in module_coverage.items():
        if addr in cov:
            cov[addr] += cnt
        else:
            cov[addr] = cnt


def _get_addr_coverage(directory, aggregated_coverage):
    """
    Extract address coverage from the JSON file(s) generated by the
    ``TranslationBlockCoverage`` plugin.

    Note that these addresses are an over-approximation of addresses
    actually executed because they are generated by extrapolating between
    the translation block start and end addresses. This doesn't actually
    matter, because if the address doesn't correspond to a line number in
    the DWARF information then it will just be ignored.

    Parameters:
        directory: typically the s2e-last or any s2e-out-* folders

        aggregated_coverage:
                   A dictionary mapping a module name to (over-approximated) instruction addresses
                   executed by S2E to the number of times they were executed.
                   This function can be invoked repeatedly with different
                   output folders in order to aggregate coverage information
                   from different runs. The first invocation should contain {} (empty coverage).
    """
    logger.info('Generating translation block coverage information')

    tb_files = get_tb_files(directory)
    tb_coverage_files = aggregate_tb_files(tb_files)

    # Get the number of times each address was executed by S2E
    for module_path, coverage in tb_coverage_files.items():
        addr_counts = {}

        for start_addr, _, size in coverage:
            # TODO: it's better to use an interval map instead
            for byte in range(0, size):
                addr = start_addr + byte
                # The coverage files we get do not indicate how many times an bb has been
                # executed. It's more of an approximation of how many times
                # the block was translated. To avoid confusion, always set execution
                # count to 1.
                addr_counts[addr] = 1

        _merge_coverage(aggregated_coverage, module_path, addr_counts)

    return aggregated_coverage


def _save_coverage_info(lcov_path, file_line_info, ignore_missing_files):
    """
    Save the line coverage information in lcov format.

    The lcov format is described here:
    http://ltp.sourceforge.net/coverage/lcov/geninfo.1.php

    Args:
        file_line_info: The file line dictionary created by
                        ``_get_file_line_coverage``.

    Returns:
        The file path where the line coverage information was written to.
    """
    logger.info('Writing line coverage to %s', lcov_path)

    with open(lcov_path, 'w') as f:
        f.write('TN:\n')
        for src_file in file_line_info:
            if ignore_missing_files and not os.path.exists(src_file):
                logger.warning('%s does not exist, skipping', src_file)
                continue

            logger.info(src_file)

            num_non_zero_lines = 0
            num_instrumented_lines = 0

            f.write('SF:%s\n' % src_file)
            for line, count in file_line_info[src_file].items():
                f.write('DA:%d,%d\n' % (line, count))

                if count:
                    num_non_zero_lines += 1
                num_instrumented_lines += 1
            f.write('LH:%d\n' % num_non_zero_lines)
            f.write('LF:%d\n' % num_instrumented_lines)
            f.write('end_of_record\n')


def _gen_html(lcov_info_path, lcov_html_dir):
    """
    Generate an LCOV HTML report.

    Returns the directory containing the HTML report.
    """
    try:
        genhtml(lcov_info_path, output_directory=lcov_html_dir,
                _out=sys.stdout, _err=sys.stderr, _fg=True)
    except ErrorReturnCode as e:
        raise CommandError(e)


class LineCoverage(ProjectCommand):
    """
    Generate a line coverage report.

    This line coverage report is in the `lcov
    <http://ltp.sourceforge.net/coverage/lcov.php>` format, so it can be used
    to generate HTML reports.
    """

    help = 'Generates a line coverage report. Requires that the binary has ' \
           'compiled with debug information and that the source code is '    \
           'available. This command supports aggregating coverage across several runs.'

    # pylint: disable=too-many-locals
    def handle(self, *args, **options):
        do_gen_html = options.get('html', False)
        syms = SymbolManager(self.install_path(), self.symbol_search_path)

        lcov_out_dir = options.get('lcov_out_dir')
        if not lcov_out_dir:
            lcov_out_dir = self.project_path('s2e-last')

        coverage = {}
        for directory in self._get_output_folders(**options):
            logger.info('Extracting coverage info from %s...', directory)
            _get_addr_coverage(directory, coverage)

        for module_path, addr_counts in coverage.items():
            try:
                cov = options.get('include_covered_files_only', False)
                file_line_info = syms.get_coverage(module_path, addr_counts, cov)

                module = os.path.basename(module_path)

                # genhtml will throw an error if there are missing files, so we skip them
                # if the user enabled html reports.
                lcov_info_path = os.path.join(lcov_out_dir, module + '.info')
                _save_coverage_info(lcov_info_path, file_line_info, do_gen_html)

                logger.success('Line coverage saved to %s', lcov_info_path)

                if do_gen_html:
                    lcov_html_dir = os.path.join(lcov_out_dir, '%s_lcov' % module)
                    _gen_html(lcov_info_path, lcov_html_dir)
                    logger.success('An HTML report is available in %s/index.html', lcov_html_dir)

            except Exception as e:
                logger.error(e)
                continue

    def _get_output_folders(self, **options):
        """
        Returns a list of S2E output folders to aggregate for code coverage.

        - The --aggregate-outputs option specifies the last n s2e-out-xxx folders to use (starting from s2e-last)
        - Alternatively, one or more --s2e-out-dir options allow picking specific output folders
        """
        last_n_outputs = options.get('aggregate_outputs', 0)
        if last_n_outputs:
            s2e_last = os.path.realpath(self.project_path('s2e-last'))
            dirname = os.path.dirname(s2e_last)
            base = os.path.basename(s2e_last)
            _, _, index = base.split('-')

            ret = []
            for i in range(0, last_n_outputs):
                output_dir = os.path.join(dirname, 's2e-out-%d' % (int(index) - i))
                if os.path.exists(output_dir):
                    ret.append(output_dir)

            return ret

        directories = options.get('s2e_out_dir', [])
        if not directories:
            return [self.project_path('s2e-last')]
        return directories