-- ============================================================================
--  jx-lab-trace · 建表 DDL（MySQL 8.0）
--  设计依据：docs/01-设计定案.md（v0.6 起）· docs/02-追踪码规则.md（v1 冻结）
--  共 37 张表。字符集 utf8mb4，引擎 InnoDB。
--
--  ★ 贯穿性原则（见设计定案 §2）：
--    P1 数据不可变 —— 业务事实字段【只写不改】；修正走「作废 + 新增 + supersedes 链」。
--       状态字段（status）允许流转，因为它表达「当前处于哪一步」而非「当时是什么」。
--    P2 码携带身份、关系存库 —— 多对多谱系一律落关系表，不编进码。
--    P4 报表层只读业务表，绝不写。
--
--  ★ 命名约定：m_ 主数据 · b_ 业务 · s_ 系统
--  ★ 每个业务表都带 created_at / created_by；主数据另带版本链与 external_id
-- ============================================================================

SET NAMES utf8mb4;
SET FOREIGN_KEY_CHECKS = 0;

-- ============================================================================
-- 一、主数据（8 张）
-- ============================================================================

CREATE TABLE m_customer (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          CHAR(4)         NOT NULL COMMENT '4 位数字客户编号，全局唯一',
  name          VARCHAR(128)    NOT NULL,
  short_name    VARCHAR(64)     NULL,
  contact       VARCHAR(64)     NULL,
  phone         VARCHAR(32)     NULL,
  is_internal   TINYINT(1)      NOT NULL DEFAULT 0 COMMENT '是否集团内',
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  external_id   VARCHAR(64)     NULL COMMENT '为日后 ERP 接管留主键映射位',
  version       INT             NOT NULL DEFAULT 1,
  supersedes_id BIGINT UNSIGNED NULL,
  is_current    TINYINT(1)      NOT NULL DEFAULT 1,
  valid_from    DATETIME(3)     NULL,
  valid_to      DATETIME(3)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_customer_code (code),
  KEY idx_customer_current (is_current, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='客户档案（独立模块，后台增删改查）';

CREATE TABLE m_composition (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          VARCHAR(16)     NOT NULL COMMENT '原料组成（荆门焦、长岭焦…）',
  name          VARCHAR(64)     NOT NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  external_id   VARCHAR(64)     NULL,
  version       INT             NOT NULL DEFAULT 1,
  supersedes_id BIGINT UNSIGNED NULL,
  is_current    TINYINT(1)      NOT NULL DEFAULT 1,
  valid_from    DATETIME(3)     NULL,
  valid_to      DATETIME(3)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_composition_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='原料组成字典';

CREATE TABLE m_material_type (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          VARCHAR(16)     NOT NULL COMMENT '原料类型（煅后焦、半煅焦…）',
  name          VARCHAR(64)     NOT NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  external_id   VARCHAR(64)     NULL,
  version       INT             NOT NULL DEFAULT 1,
  supersedes_id BIGINT UNSIGNED NULL,
  is_current    TINYINT(1)      NOT NULL DEFAULT 1,
  valid_from    DATETIME(3)     NULL,
  valid_to      DATETIME(3)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_mtype_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='原料类型字典';

CREATE TABLE m_material (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code            CHAR(4)         NOT NULL COMMENT '4 位物料编号，全局唯一',
  name            VARCHAR(128)    NOT NULL,
  composition_id  BIGINT UNSIGNED NULL COMMENT '原料组成 FK',
  material_type_id BIGINT UNSIGNED NULL COMMENT '原料类型 FK',
  kind            VARCHAR(16)     NOT NULL COMMENT '原料 / 成品',
  spec            VARCHAR(128)    NULL COMMENT '规格',
  unit            VARCHAR(16)     NOT NULL DEFAULT 't',
  status          VARCHAR(16)     NOT NULL DEFAULT '启用',
  external_id     VARCHAR(64)     NULL,
  version         INT             NOT NULL DEFAULT 1,
  supersedes_id   BIGINT UNSIGNED NULL,
  is_current      TINYINT(1)      NOT NULL DEFAULT 1,
  valid_from      DATETIME(3)     NULL,
  valid_to        DATETIME(3)     NULL,
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by      VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_material_code (code),
  KEY idx_material_kind (kind, is_current)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='物料档案（原料/成品统一建模）';

CREATE TABLE m_test_item (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          VARCHAR(32)     NOT NULL COMMENT '检测项目编码',
  name          VARCHAR(64)     NOT NULL,
  unit          VARCHAR(16)     NULL,
  method        VARCHAR(128)    NULL COMMENT '检测方法 / 标准号',
  value_type    VARCHAR(16)     NOT NULL DEFAULT '数值' COMMENT '数值 / 文本 / 枚举',
  decimals      TINYINT         NOT NULL DEFAULT 2,
  enum_values   VARCHAR(255)    NULL COMMENT 'value_type=枚举 时的候选集，逗号分隔',
  sort          INT             NOT NULL DEFAULT 0,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  external_id   VARCHAR(64)     NULL,
  version       INT             NOT NULL DEFAULT 1,
  supersedes_id BIGINT UNSIGNED NULL,
  is_current    TINYINT(1)      NOT NULL DEFAULT 1,
  valid_from    DATETIME(3)     NULL,
  valid_to      DATETIME(3)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_test_item_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='检测项目字典（后台可维护的系统常量）';

-- ★ 判定限按「客户 × 物料」维度配置，不是一张全局表
CREATE TABLE m_test_item_limit (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  test_item_id  BIGINT UNSIGNED NOT NULL,
  customer_id   BIGINT UNSIGNED NOT NULL COMMENT '0 = 通用默认',
  material_id   BIGINT UNSIGNED NOT NULL COMMENT '0 = 通用默认',
  lower_limit   DECIMAL(18,6)   NULL,
  upper_limit   DECIMAL(18,6)   NULL,
  is_required   TINYINT(1)      NOT NULL DEFAULT 0,
  note          VARCHAR(255)    NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  version       INT             NOT NULL DEFAULT 1,
  supersedes_id BIGINT UNSIGNED NULL,
  is_current    TINYINT(1)      NOT NULL DEFAULT 1,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_limit_scope (test_item_id, customer_id, material_id, version),
  KEY idx_limit_lookup (test_item_id, customer_id, material_id, is_current)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='检测项判定限（按 客户 × 物料 配置）';

CREATE TABLE m_vehicle (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  plate_no      VARCHAR(32)     NOT NULL COMMENT '车牌号',
  default_driver VARCHAR(64)    NULL,
  default_phone VARCHAR(32)     NULL,
  carrier       VARCHAR(128)    NULL COMMENT '承运商',
  note          VARCHAR(255)    NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  external_id   VARCHAR(64)     NULL,
  version       INT             NOT NULL DEFAULT 1,
  supersedes_id BIGINT UNSIGNED NULL,
  is_current    TINYINT(1)      NOT NULL DEFAULT 1,
  valid_from    DATETIME(3)     NULL,
  valid_to      DATETIME(3)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_vehicle_plate (plate_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='车辆档案（主数据，长期存在）';

-- ★ 人员与班组不适用「版本链」语义（来去不是版本），故不带 version/supersedes
CREATE TABLE m_team (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          VARCHAR(16)     NOT NULL,
  name          VARCHAR(64)     NOT NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_team_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='班组（不适用版本链）';

CREATE TABLE m_user (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  open_id       VARCHAR(64)     NOT NULL COMMENT '飞书 open_id，身份主键',
  name          VARCHAR(64)     NOT NULL,
  dept          VARCHAR(128)    NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_open_id (open_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户（飞书 open_id 为身份主键；不适用版本链）';

-- ============================================================================
-- 二、收货与打码（5 张）
-- ============================================================================

CREATE TABLE b_arrival_notice (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  notice_no     VARCHAR(32)     NOT NULL COMMENT '预报单号',
  customer_id   BIGINT UNSIGNED NOT NULL,
  material_id   BIGINT UNSIGNED NOT NULL,
  vehicle_id    BIGINT UNSIGNED NULL,
  plate_no      VARCHAR(32)     NULL,
  driver        VARCHAR(64)     NULL,
  phone         VARCHAR(32)     NULL,
  eta           DATETIME(3)     NULL COMMENT '预计到货时间',
  est_bag_count INT             NULL COMMENT '预计袋数（仅预报用，不据此打码）',
  biz_type      CHAR(2)         NOT NULL DEFAULT 'CG' COMMENT 'CG 客供 / ZG 自购',
  seq_no        SMALLINT UNSIGNED NULL COMMENT '预分配的车序（空号跳号不回收）',
  arrive_date   DATE            NULL COMMENT '链根日期：到货日',
  status        VARCHAR(16)     NOT NULL DEFAULT '预报' COMMENT '预报 / 已到货 / 已取消 / 空号',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_notice_no (notice_no),
  KEY idx_notice_seq (customer_id, material_id, arrive_date, seq_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='预报单（客户提前告知的车辆信息）';

CREATE TABLE b_truck_lot (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          CHAR(27)        NOT NULL COMMENT '车次码（T=A）',
  notice_id     BIGINT UNSIGNED NULL,
  customer_id   BIGINT UNSIGNED NOT NULL,
  material_id   BIGINT UNSIGNED NOT NULL,
  vehicle_id    BIGINT UNSIGNED NULL,
  driver        VARCHAR(64)     NULL,
  phone         VARCHAR(32)     NULL,
  biz_type      CHAR(2)         NOT NULL DEFAULT 'CG',
  arrive_date   DATE            NOT NULL,
  arrive_at     DATETIME(3)     NOT NULL,
  gross_weight  DECIMAL(18,3)   NULL COMMENT '毛重（吨）',
  tare_weight   DECIMAL(18,3)   NULL COMMENT '皮重（吨）',
  net_weight    DECIMAL(18,3)   NULL COMMENT '净重 = 毛重 − 皮重（实测）',
  bag_count     INT             NOT NULL DEFAULT 0 COMMENT '有效袋数（作废袋不计）',
  status        VARCHAR(16)     NOT NULL DEFAULT '待检' COMMENT '待检 / 合格 / 不合格 / 让步接收 / 已退货 / 已作废',
  remark        VARCHAR(255)    NULL,
  operator      VARCHAR(64)     NOT NULL DEFAULT '',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_truck_code (code),
  KEY idx_truck_cust_date (customer_id, arrive_date),
  KEY idx_truck_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='车次（一次到货事件）';

CREATE TABLE b_bag (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code               CHAR(27)        NOT NULL COMMENT '吨袋码（T=B）',
  truck_lot_id       BIGINT UNSIGNED NOT NULL,
  bag_seq            SMALLINT UNSIGNED NOT NULL,
  weight_allocated   DECIMAL(18,3)   NULL COMMENT '摊算重量 = 车净重 ÷ 袋数',
  weight_is_allocated TINYINT(1)     NOT NULL DEFAULT 1 COMMENT '★ 恒为 1：重量是摊算值，不是实测',
  status             VARCHAR(16)     NOT NULL DEFAULT '在库' COMMENT '在库 / 已投料 / 已退回 / 留样中 / 作废',
  created_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by         VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_bag_code (code),
  UNIQUE KEY uk_bag_seq (truck_lot_id, bag_seq),
  KEY idx_bag_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='原料吨袋（收货/投料的最小单位）';

CREATE TABLE b_label_print (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          CHAR(27)        NOT NULL COMMENT '被打印的对象码',
  printed_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  printed_by    VARCHAR(64)     NOT NULL DEFAULT '',
  is_reprint    TINYINT(1)      NOT NULL DEFAULT 0,
  reason        VARCHAR(255)    NULL COMMENT '★ 补打必须填原因，否则会出现「一物两码」',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_label_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='标签打印记录（含补打留痕）';

-- ★ 通用作废记录：袋 / 批 / 单据的作废统一走这张表，被作废对象的 status 置为「作废」
CREATE TABLE b_obj_void (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  entity        VARCHAR(64)     NOT NULL COMMENT '表名',
  entity_id     BIGINT UNSIGNED NOT NULL,
  reason        VARCHAR(255)    NOT NULL,
  voided_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  voided_by     VARCHAR(64)     NOT NULL DEFAULT '',
  approved_by   VARCHAR(64)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_void_entity (entity, entity_id),
  KEY idx_void_at (voided_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='通用作废记录（作废码永久不重用）';

-- ============================================================================
-- 三、生产与谱系（6 张）
-- ============================================================================

CREATE TABLE b_production_batch (
  id                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code                      CHAR(27)        NOT NULL COMMENT '生产批码（T=C）',
  customer_id               BIGINT UNSIGNED NOT NULL,
  input_material_id         BIGINT UNSIGNED NOT NULL COMMENT '投入物料',
  planned_output_material_id BIGINT UNSIGNED NOT NULL COMMENT '★ 计划产出物料（用于批号编排与排产）',
  batch_date                DATE            NOT NULL COMMENT '链根日期：生产批创建日',
  status                    VARCHAR(16)     NOT NULL DEFAULT '进行中' COMMENT '进行中 / 已完成 / 已作废',
  remark                    VARCHAR(255)    NULL,
  created_at                DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by                VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_batch_code (code),
  KEY idx_batch_cust_date (customer_id, batch_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='生产批（投料与作业的过程单元；★不含实际产出）';

CREATE TABLE b_batch_operation (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  batch_id      BIGINT UNSIGNED NOT NULL,
  seq           SMALLINT UNSIGNED NOT NULL,
  team_id       BIGINT UNSIGNED NULL,
  operator      VARCHAR(64)     NULL,
  start_at      DATETIME(3)     NULL,
  end_at        DATETIME(3)     NULL,
  output_weight DECIMAL(18,3)   NULL COMMENT '本作业段产出（吨），非整批产出',
  remark        VARCHAR(255)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_op_seq (batch_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='作业段（★承载「跨班组」）';

-- ★★ 谱系表：本系统的承重墙。正向/反向追溯都靠它。
CREATE TABLE b_feed_record (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  batch_id      BIGINT UNSIGNED NOT NULL,
  bag_id        BIGINT UNSIGNED NOT NULL,
  feed_weight   DECIMAL(18,3)   NULL COMMENT '投料量（摊算）',
  fed_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  operator      VARCHAR(64)     NOT NULL DEFAULT '',
  remark        VARCHAR(255)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_feed_batch (batch_id),
  KEY idx_feed_bag (bag_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='投料记录（★谱系承重墙：多对多，绝不编进码）';

CREATE TABLE b_fg_lot (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code              CHAR(27)        NOT NULL COMMENT '成品批码（T=D）',
  batch_id          BIGINT UNSIGNED NOT NULL COMMENT '来源生产批',
  customer_id       BIGINT UNSIGNED NOT NULL,
  output_material_id BIGINT UNSIGNED NOT NULL COMMENT '★ 实际产出物料（唯一记录点）',
  pack_spec         VARCHAR(64)     NULL COMMENT '包装规格',
  qty_bag           INT             NOT NULL DEFAULT 0,
  net_weight        DECIMAL(18,3)   NULL COMMENT '实际产出净重（吨）',
  produced_at       DATETIME(3)     NULL,
  status            VARCHAR(16)     NOT NULL DEFAULT '在库' COMMENT '在库 / 已出货 / 已作废',
  remark            VARCHAR(255)    NULL,
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by        VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_fglot_code (code),
  KEY idx_fglot_batch (batch_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='成品批（可交付的产出分段；★实际产出唯一记录点）';

CREATE TABLE b_fg_bag (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code               CHAR(27)        NOT NULL COMMENT '成品吨袋码（T=E）',
  fg_lot_id          BIGINT UNSIGNED NOT NULL,
  bag_seq            SMALLINT UNSIGNED NOT NULL,
  weight_allocated   DECIMAL(18,3)   NULL,
  weight_is_allocated TINYINT(1)     NOT NULL DEFAULT 1,
  status             VARCHAR(16)     NOT NULL DEFAULT '在库' COMMENT '在库 / 已出厂 / 作废',
  created_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by         VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_fgbag_code (code),
  UNIQUE KEY uk_fgbag_seq (fg_lot_id, bag_seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='成品吨袋（出场逐袋扫码）';

CREATE TABLE b_rework (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  new_batch_id  BIGINT UNSIGNED NOT NULL COMMENT '★ 返工用新批号',
  src_batch_id  BIGINT UNSIGNED NOT NULL COMMENT '关联原批',
  reason        VARCHAR(255)    NOT NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_rework_new (new_batch_id),
  KEY idx_rework_src (src_batch_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='返工关联';

-- ============================================================================
-- 四、取样、检测、留样（8 张）
--    ★ 模型依据国标取样路径：份样 → 大样 → 检测，且检测样与保留样物理分开
-- ============================================================================

CREATE TABLE b_sample_group (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  group_no      VARCHAR(40)     NOT NULL COMMENT '取样组号（大样）',
  target_type   VARCHAR(16)     NOT NULL COMMENT '车次 / 生产批 / 成品批',
  target_id     BIGINT UNSIGNED NOT NULL,
  sample_count  INT             NOT NULL DEFAULT 0 COMMENT '份样数',
  sampled_at    DATETIME(3)     NULL,
  sampled_by    VARCHAR(64)     NULL,
  remark        VARCHAR(255)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_group_no (group_no),
  KEY idx_group_target (target_type, target_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='取样组（大样）';

CREATE TABLE b_sample (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  sample_no     VARCHAR(64)     NOT NULL COMMENT '派生串：父码 + 角色码 + 2位序号',
  role          VARCHAR(16)     NOT NULL COMMENT '份样 / 大样 / 保留样 / 仲裁样',
  group_id      BIGINT UNSIGNED NULL COMMENT '所属取样组',
  bag_id        BIGINT UNSIGNED NULL COMMENT '原料样：绑吨袋',
  batch_id      BIGINT UNSIGNED NULL COMMENT '中间样：绑生产批',
  fg_lot_id     BIGINT UNSIGNED NULL COMMENT '成品样：绑成品批',
  sampled_at    DATETIME(3)     NULL,
  sampled_by    VARCHAR(64)     NULL,
  sample_weight DECIMAL(18,3)   NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '在库',
  remark        VARCHAR(255)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_sample_no (sample_no),
  KEY idx_sample_group (group_id),
  KEY idx_sample_bag (bag_id),
  KEY idx_sample_batch (batch_id),
  KEY idx_sample_fglot (fg_lot_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='样品（份样/大样/保留样/仲裁样）';

CREATE TABLE b_sample_retention (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  sample_id       BIGINT UNSIGNED NOT NULL,
  location        VARCHAR(128)    NOT NULL COMMENT '三层文本，如「化验室-留样柜A-第3层」',
  stored_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  retention_until DATE            NULL COMMENT '保留期限（可配置：原料6月/中间3月/成品≥1年/仲裁≥2年）',
  status          VARCHAR(16)     NOT NULL DEFAULT '在库' COMMENT '在库 / 已借出 / 已销毁',
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by      VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_retention_sample (sample_id),
  KEY idx_retention_until (retention_until, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='留样（位置 / 保留期限）';

CREATE TABLE b_sample_lend (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  sample_id     BIGINT UNSIGNED NOT NULL,
  lent_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  lent_to       VARCHAR(64)     NOT NULL DEFAULT '',
  purpose       VARCHAR(255)    NULL,
  returned_at   DATETIME(3)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_lend_sample (sample_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='留样借还记录（★独立表，不用 JSON 字段 —— 与 P1 冲突）';

CREATE TABLE b_sample_destroy (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  sample_id     BIGINT UNSIGNED NOT NULL,
  destroyed_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  destroyed_by  VARCHAR(64)     NOT NULL DEFAULT '',
  approved_by   VARCHAR(64)     NOT NULL COMMENT '★ 销毁必须留谁批的',
  reason        VARCHAR(255)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_destroy_sample (sample_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='留样销毁记录';

CREATE TABLE b_inspection (
  id                    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  inspection_no         VARCHAR(40)     NOT NULL,
  target_type           VARCHAR(16)     NOT NULL COMMENT '车次 / 生产批 / 成品批',
  target_id             BIGINT UNSIGNED NOT NULL,
  group_id              BIGINT UNSIGNED NULL COMMENT '★ 检测挂「大样」，不挂袋',
  test_date             DATE            NULL,
  inspector             VARCHAR(64)     NULL,
  method                VARCHAR(16)     NULL COMMENT '全检 / 抽检',
  conclusion            VARCHAR(16)     NULL COMMENT '合格 / 不合格 / CONCESSION（界面显示「让步接收」）',
  defect_desc           VARCHAR(500)    NULL,
  disposition           VARCHAR(16)     NULL COMMENT '退货 / 换货 / 让步接收 / 返工',
  authorized_by         VARCHAR(64)     NULL COMMENT '★ 让步授权人（ISO 9001 8.7.2 强制）',
  qc_signed_by          VARCHAR(64)     NULL COMMENT '让步接收 · 质检方签署',
  dept_signed_by        VARCHAR(64)     NULL COMMENT '让步接收 · 使用部门签署',
  cust_notified_at      DATETIME(3)     NULL COMMENT '★ 让步接收必填：何时告知客户',
  cust_contact          VARCHAR(64)     NULL COMMENT '★ 让步接收必填：告知谁',
  cust_channel          VARCHAR(16)     NULL COMMENT '★ 让步接收必填：电话/微信/邮件/书面',
  cust_confirm_file_id  BIGINT UNSIGNED NULL COMMENT '客户书面确认（可选）',
  is_recheck            TINYINT(1)      NOT NULL DEFAULT 0,
  recheck_of            BIGINT UNSIGNED NULL COMMENT '复检所针对的原检测单',
  remark                VARCHAR(255)    NULL,
  created_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by            VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_inspection_no (inspection_no),
  KEY idx_insp_target (target_type, target_id),
  KEY idx_insp_group (group_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='检测单（挂大样）';

CREATE TABLE b_inspection_result (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  inspection_id BIGINT UNSIGNED NOT NULL,
  item_id       BIGINT UNSIGNED NOT NULL,
  state         VARCHAR(16)     NOT NULL DEFAULT '未测' COMMENT '★ 未测(待办) / 已测 / 不适用(结论) —— 三态不可合并',
  value_num     DECIMAL(18,6)   NULL,
  value_text    VARCHAR(255)    NULL,
  unit          VARCHAR(16)     NULL,
  judge         VARCHAR(16)     NULL COMMENT '合格 / 不合格',
  source        VARCHAR(16)     NOT NULL DEFAULT '人工' COMMENT '人工 / 仪器',
  lower_limit   DECIMAL(18,6)   NULL COMMENT '取值时点的判定限快照',
  upper_limit   DECIMAL(18,6)   NULL,
  remark        VARCHAR(255)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_result_item (inspection_id, item_id),
  KEY idx_result_state (inspection_id, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='检测结果（★批次检测项清单 = 本表的行集合，含「未测」行）';

CREATE TABLE b_inspection_file (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  inspection_id BIGINT UNSIGNED NOT NULL,
  file_name     VARCHAR(255)    NOT NULL,
  file_path     VARCHAR(512)    NOT NULL COMMENT '只存路径，附件不进数据库',
  file_type     VARCHAR(64)     NULL,
  file_size     BIGINT UNSIGNED NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_ifile_insp (inspection_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='检测附件';

-- ============================================================================
-- 五、出货、报告、审计（5 张）
-- ============================================================================

CREATE TABLE b_shipment (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  shipment_no   VARCHAR(40)     NOT NULL,
  customer_id   BIGINT UNSIGNED NOT NULL,
  vehicle_id    BIGINT UNSIGNED NULL,
  plate_no      VARCHAR(32)     NULL,
  driver        VARCHAR(64)     NULL,
  ship_at       DATETIME(3)     NULL,
  operator      VARCHAR(64)     NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '已出厂' COMMENT '已出厂 / 已撤销',
  remark        VARCHAR(255)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_shipment_no (shipment_no),
  KEY idx_ship_customer (customer_id, ship_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='出货单';

CREATE TABLE b_shipment_item (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  shipment_id   BIGINT UNSIGNED NOT NULL,
  fg_bag_id     BIGINT UNSIGNED NOT NULL COMMENT '逐袋扫码归集',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_ship_bag (shipment_id, fg_bag_id),
  KEY idx_shipitem_bag (fg_bag_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='出货明细';

CREATE TABLE b_share_report (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  report_no     VARCHAR(40)     NOT NULL,
  title         VARCHAR(255)    NULL,
  scope_type    VARCHAR(16)     NULL COMMENT '按批次 / 按车次',
  scope_json    TEXT            NULL COMMENT '所选范围',
  snapshot_path VARCHAR(512)    NULL COMMENT '静态快照路径（公网侧只放静态文件，不连内网库）',
  url           VARCHAR(512)    NULL,
  token         VARCHAR(64)     NOT NULL COMMENT '★ 长随机串；不做访问隔离，但不可猜测',
  generated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  expires_at    DATETIME(3)     NULL COMMENT '生命周期，后台可设',
  generated_by  VARCHAR(64)     NULL,
  status        VARCHAR(16)     NOT NULL DEFAULT '有效' COMMENT '有效 / 已撤销 / 已过期',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_report_no (report_no),
  UNIQUE KEY uk_report_token (token),
  KEY idx_report_expire (expires_at, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='分享报告（★刷新＝新 token 新链接）';

CREATE TABLE b_share_access (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  report_id     BIGINT UNSIGNED NOT NULL,
  accessed_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  ip            VARCHAR(64)     NULL,
  ua            VARCHAR(512)    NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_access_report (report_id, accessed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='报告访问日志';

CREATE TABLE s_audit_log (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  entity        VARCHAR(64)     NOT NULL,
  entity_id     BIGINT UNSIGNED NULL,
  action        VARCHAR(32)     NOT NULL COMMENT 'create / supersede / void / status / perm_change ...',
  field         VARCHAR(64)     NULL,
  old_value     TEXT            NULL,
  new_value     TEXT            NULL,
  actor_open_id VARCHAR(64)     NULL,
  actor_name    VARCHAR(64)     NULL,
  actor_role    VARCHAR(64)     NULL,
  ip            VARCHAR(64)     NULL,
  reason        VARCHAR(255)    NULL,
  at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  KEY idx_audit_entity (entity, entity_id),
  KEY idx_audit_at (at),
  KEY idx_audit_actor (actor_open_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='审计日志（只增不改）';

-- ============================================================================
-- 六、权限与配置（5 张）  ★ 数据驱动、后台可配
-- ============================================================================

CREATE TABLE s_permission_point (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          VARCHAR(64)     NOT NULL,
  module        VARCHAR(32)     NOT NULL,
  name          VARCHAR(128)    NOT NULL,
  levels        VARCHAR(128)    NOT NULL COMMENT '该点支持的级别集合，如 ALL,READ,NONE',
  scope_kind    VARCHAR(32)     NULL COMMENT '预留：二期行级/列级权限',
  sort          INT             NOT NULL DEFAULT 0,
  is_system     TINYINT(1)      NOT NULL DEFAULT 1 COMMENT '★ 由代码注册，后台不可增删',
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_perm_code (code),
  KEY idx_perm_module (module, sort)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='权限点字典（★由代码注册，后台不可增删）';

CREATE TABLE s_role (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  code          VARCHAR(32)     NOT NULL,
  name          VARCHAR(64)     NOT NULL,
  kind          VARCHAR(16)     NOT NULL DEFAULT 'business' COMMENT 'business / system',
  is_system     TINYINT(1)      NOT NULL DEFAULT 0 COMMENT '★ 内置角色不可删除',
  sort          INT             NOT NULL DEFAULT 0,
  status        VARCHAR(16)     NOT NULL DEFAULT '启用',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_role_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='角色';

CREATE TABLE s_role_permission (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  role_code     VARCHAR(32)     NOT NULL,
  point_code    VARCHAR(64)     NOT NULL,
  level         VARCHAR(16)     NOT NULL COMMENT 'ALL / READ / INIT / APPROVE / NONE',
  updated_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  updated_by    VARCHAR(64)     NOT NULL DEFAULT '',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_role_point (role_code, point_code),
  KEY idx_rp_point (point_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='角色授权';

-- ★ UNIQUE(open_id, role_code) 而非 open_id 单列唯一 —— 一账号可叠加多角色
CREATE TABLE s_user_role (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  open_id       VARCHAR(64)     NOT NULL,
  role_code     VARCHAR(32)     NOT NULL,
  granted_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  granted_by    VARCHAR(64)     NOT NULL DEFAULT '',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_role (open_id, role_code),
  KEY idx_ur_role (role_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户角色绑定（可叠加）';

-- ★ 会话必须落库 + 滑动续期（不照抄采购平台的「内存会话」——重启即全员掉线）
CREATE TABLE s_session (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  session_id    VARCHAR(64)     NOT NULL,
  open_id       VARCHAR(64)     NOT NULL,
  issued_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  expires_at    DATETIME(3)     NOT NULL,
  last_seen_at  DATETIME(3)     NULL COMMENT '滑动续期用',
  ip            VARCHAR(64)     NULL,
  revoked_at    DATETIME(3)     NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  created_by    VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_session_id (session_id),
  KEY idx_session_open (open_id),
  KEY idx_session_expire (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会话（落库 + 滑动续期）';

SET FOREIGN_KEY_CHECKS = 1;

-- ============================================================================
--  校验视图（可选）：一段式自检 —— 表数应为 37
-- ============================================================================
-- SELECT COUNT(*) AS table_count FROM information_schema.tables
--  WHERE table_schema = DATABASE();
