% erplab_erp.m: the ERP and its SME computed by ERPLAB, for check_erplab.py.
%
% Run by check_erplab.py in GNU Octave, with EEGLAB's and ERPLAB's functions
% on the path. Reads input.mat (the continuous recording, already low-passed
% the way NeuroHalo does it, and its markers as numeric codes 1, 2, ...),
% then does what an ERPLAB user does after filtering:
%
%   pop_creabasiceventlist -> pop_binlister (one bin per marker label)
%   -> pop_epochbin (-200 to 800 ms, baseline 'pre')
%   -> pop_averager (data quality on: ERPLAB's default aSME windows)
%
% once with every trial kept, and then once per sensor with ERPLAB's
% moving-window peak-to-peak test on that sensor only, so trials are left
% out per sensor as NeuroHalo does. Writes output.mat.

function erplab_erp(workdir)
  in = load(fullfile(workdir, 'input.mat'));
  labels = cellstr(in.labels);   % scipy writes the labels as a cell array
  nlabels = numel(labels);
  nchan = size(in.data, 1);

  EEG = eeg_emptyset();
  EEG.data = double(in.data);
  EEG.srate = double(in.srate);
  EEG.nbchan = nchan;
  EEG.pnts = size(in.data, 2);
  EEG.trials = 1;
  EEG.xmin = 0;
  EEG.xmax = (EEG.pnts - 1) / EEG.srate;
  EEG.chanlocs = struct('labels', cellstr(in.channels));
  EEG.event = struct('type', num2cell(double(in.codes(:)')), 'latency', num2cell(double(in.latencies(:)')));
  EEG = eeg_checkset(EEG);

  % One bin per marker label: bin k holds the markers with code k.
  bdf = fullfile(workdir, 'bins.txt');
  f = fopen(bdf, 'w');
  for k = 1:nlabels
    fprintf(f, 'bin %d\n%s\n.{%d}\n\n', k, strtrim(labels{k}), k);
  end
  fclose(f);

  EEG = pop_creabasiceventlist(EEG, 'AlphanumericCleaning', 'on', 'BoundaryNumeric', {-99}, ...
    'BoundaryString', {'boundary'}, 'Warning', 'off');
  EEG = pop_binlister(EEG, 'BDF', bdf, 'IndexEL', 1, 'SendEL2', 'EEG', 'Voutput', 'EEG', 'Warning', 'off');
  % EEGLAB's epochs leave out their end point, so -200 to 800 ms would stop
  % at the sample before 800 ms; one sample more keeps 800 ms in, as
  % NeuroHalo and MNE do.
  EEG = pop_epochbin(EEG, [-200, 800 + 1000 / EEG.srate], 'pre');

  % Every trial kept.
  ERP = pop_averager(EEG, 'Criterion', 'all', 'DQ_flag', 1, 'Warning', 'off');
  keep_all.mean = ERP.bindata;                       % sensors x samples x bins
  keep_all.sme = aSME(ERP);                          % sensors x windows x bins
  keep_all.accepted = ERP.ntrials.accepted;
  times = ERP.times;
  windows = ERP.dataquality(strcmpi({ERP.dataquality.type}, 'aSME')).times;

  % Per sensor: trials whose peak-to-peak on that sensor is over the
  % threshold are left out of that sensor's average.
  reject.threshold = double(in.reject);
  reject.mean = zeros(size(keep_all.mean));
  reject.sme = zeros(size(keep_all.sme));
  reject.accepted = zeros(nchan, nlabels);
  span = round((EEG.xmax - EEG.xmin) * 1000);        % one window over the whole epoch
  for c = 1:nchan
    marked = pop_artmwppth(EEG, 'Channel', c, 'Threshold', reject.threshold, ...
      'Twindow', [-200 800], 'Windowsize', span, 'Windowstep', span, 'Flag', 1, 'Review', 'off');
    E = pop_averager(marked, 'Criterion', 'good', 'DQ_flag', 1, 'Warning', 'off');
    reject.accepted(c, :) = E.ntrials.accepted;
    for b = 1:nlabels
      if E.ntrials.accepted(b) > 0
        reject.mean(c, :, b) = E.bindata(c, :, b);
      end
    end
    s = aSME(E);
    reject.sme(c, :, :) = s(c, :, :);
  end

  save('-v7', fullfile(workdir, 'output.mat'), 'times', 'windows', 'keep_all', 'reject');
end

% aSME returns the ERP's aSME data quality: sensors x windows x bins.
function s = aSME(ERP)
  s = ERP.dataquality(strcmpi({ERP.dataquality.type}, 'aSME')).data;
end
