/* ══════════════ Constants ══════════════ */
const TOTAL_STEPS = 3;
const CH_TOTAL_STEPS = 3;
const PROTOCOLS_BY_INTERFACE = {
  Serial: ['Modbus RTU'],
  Network: ['Modbus TCP', 'Modbus RTU']
};
const DT_LABEL = { bool:'布尔', int:'整数', float:'浮点数', string:'字符串' };
const DT_FROM_LABEL = Object.fromEntries(Object.entries(DT_LABEL).map(([k, v]) => [v, k]));
const ACCESS_LABEL = { r:'只读', w:'只写', rw:'读写' };
const ACCESS_FROM_LABEL = { '只读':'r', '只写':'w', '读写':'rw', 'R':'r', 'W':'w', 'RW':'rw', 'r':'r', 'w':'w', 'rw':'rw' };
const CHANNEL_TYPE_LABEL = { Serial:'串口通道', Network:'网络通道' };
const CHANNEL_TYPE_ICON = { Serial:'usb-symbol', Network:'ethernet' };
const PARITY_LABEL = { None:'无', Even:'偶校验', Odd:'奇校验' };
const CSV_HEADERS = ['属性ID','属性名称','属性描述','数据类型','起始位','终止位','读写属性','偏移量','数据系数','数据单位','读功能码','写功能码','寄存器基址','寄存器偏移','寄存器数量','字节顺序'];
const CSV_FIELD_MAP = {
  '属性ID':'id','id':'id',
  '属性名称':'name','名称':'name','name':'name',
  '属性描述':'description','描述':'description','description':'description',
  '数据类型':'dataType','datatype':'dataType',
  '起始位':'startBit','startbit':'startBit',
  '终止位':'endBit','endbit':'endBit',
  '数据单位':'unit','单位':'unit','unit':'unit',
  '读写属性':'accessMode','读写':'accessMode','accessmode':'accessMode',
  '偏移量':'deltaValue','delta':'deltaValue','deltavalue':'deltaValue',
  '数据基数':'deltaValue','基数':'deltaValue','base':'deltaValue',
  '数据系数':'coefficient','系数':'coefficient','coefficient':'coefficient',
  '读功能码':'readFunctionCode','readfunctioncode':'readFunctionCode',
  '写功能码':'writeFunctionCode','writefunctioncode':'writeFunctionCode',
  '寄存器基址':'registerBase','寄存器地址':'registerBase','registerbase':'registerBase','registeraddress':'registerBase',
  '寄存器偏移':'registerOffset','位偏移':'registerOffset','registeroffset':'registerOffset','bitoffset':'registerOffset',
  '寄存器数量':'registerCount','寄存器数':'registerCount','registercount':'registerCount',
  '字节顺序':'byteOrder','byteorder':'byteOrder'
};

/* ══════════════ State ══════════════ */
const state = {
  models: [],
  hardware: {},
  settings: null,
  systemInfo: null,
  channels: [],
  editingId: null,
  channel: null,
  profile: { profileIndex:'', profileId:'', name:'', manufacturer:'', description:'', deviceType:'', deviceModel:'', ratedPower:'', interfaceType:'', protocolType:'', protocolVersion:'', maxRegisterCount: 100 },
  properties: []
};
let currentStep = 1;
let chCurrentStep = 1;
let propEditIndex = -1;
let channelEditIndex = -1;
let propModal;

